package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestLineFormat pins the one line every binary prints for -version, since
// packagers and bug reports read it by eye and scripts read it by field.
func TestLineFormat(t *testing.T) {
	defer swap(&Version, "v1.2.3")()
	defer swap(&Revision, "abc1234")()
	got := Line("vantage-writer")
	want := "vantage-writer v1.2.3 (abc1234, " + runtime.Version() + ")"
	if got != want {
		t.Errorf("Line() = %q, want %q", got, want)
	}
}

// TestRevisionFallsBackToVCSStamp: a plain `go build` in a git checkout
// stamps vcs.revision into the binary, so an unset -X should report that
// rather than "unknown" -- a contributor's binary is then still traceable.
func TestRevisionFallsBackToVCSStamp(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.modified", Value: "true"},
	}}
	if got := revision("", bi); got != "0123456789ab-dirty" {
		t.Errorf("revision from vcs stamp = %q, want the 12-char prefix marked dirty", got)
	}
	bi.Settings[1].Value = "false"
	if got := revision("", bi); got != "0123456789ab" {
		t.Errorf("revision from clean vcs stamp = %q", got)
	}
	if got := revision("set-by-ldflags", bi); got != "set-by-ldflags" {
		t.Errorf("an -X Revision must win over the vcs stamp; got %q", got)
	}
	if got := revision("", nil); got != "unknown" {
		t.Errorf("no build info at all = %q, want unknown", got)
	}
	if got := revision("", &debug.BuildInfo{}); got != "unknown" {
		t.Errorf("build info without a vcs stamp = %q, want unknown", got)
	}
}

// TestRegisterExportsOneSeries checks the gauge's name, labels and value
// against a private registry, the same way a scrape would see them.
func TestRegisterExportsOneSeries(t *testing.T) {
	defer swap(&Version, "v9")()
	defer swap(&Revision, "rev9")()
	reg := prometheus.NewRegistry()
	if err := Register(reg, "writer"); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP vantage_build_info Always 1; the labels say which build of which component is running.
# TYPE vantage_build_info gauge
vantage_build_info{component="writer",goversion="` + runtime.Version() + `",revision="rev9",version="v9"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "vantage_build_info"); err != nil {
		t.Error(err)
	}
	// A second Register of the same component is a no-op, not an error:
	// run() is called more than once per process by the daemons' tests.
	if err := Register(reg, "writer"); err != nil {
		t.Errorf("second Register: %v", err)
	}
}

func swap(p *string, v string) func() {
	old := *p
	*p = v
	return func() { *p = old }
}
