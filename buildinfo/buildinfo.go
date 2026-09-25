// Package buildinfo says which build of vantage is running: the version and
// revision stamped in at link time, the Go toolchain that compiled it, and
// the vantage_build_info gauge that exports all three to Prometheus.
//
// Both vars are set with -ldflags, which is how the Makefile and every
// Dockerfile build the binaries:
//
//	go build -ldflags "-X github.com/jp2195/vantage/buildinfo.Version=v0.1.0 \
//	    -X github.com/jp2195/vantage/buildinfo.Revision=$(git rev-parse HEAD)"
//
// A build that sets neither still reports something useful: Version stays
// "dev", and Revision falls back to the vcs.revision the go command stamps
// into any binary built inside a git checkout.
package buildinfo

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/prometheus/client_golang/prometheus"
)

// Version is the release this binary was built from. Set with -ldflags -X.
var Version = "dev"

// Revision is the source revision this binary was built from. Set with
// -ldflags -X; when empty, Rev reads the go command's own vcs stamp.
var Revision = ""

// Rev returns Revision, or the vcs.revision stamp when Revision is unset, or
// "unknown" when neither exists (a build from a tarball, or with
// -buildvcs=false, as every container build is: .dockerignore drops .git).
func Rev() string {
	bi, _ := debug.ReadBuildInfo()
	return revision(Revision, bi)
}

func revision(set string, bi *debug.BuildInfo) string {
	if set != "" {
		return set
	}
	if bi == nil {
		return "unknown"
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

// Line is what -version prints: "<binary> <version> (<revision>, <goversion>)".
func Line(binary string) string {
	return fmt.Sprintf("%s %s (%s, %s)", binary, Version, Rev(), runtime.Version())
}

// Register adds vantage_build_info{component,version,revision,goversion} 1 to
// reg. component names the daemon ("collector", "writer", "api"), so one
// Prometheus can tell which build each of its targets is running.
//
// Registering the same component twice on one registry is not an error, so
// a caller that cannot easily tell whether it already has (a daemon's run,
// called more than once by its tests) need not track it.
func Register(reg prometheus.Registerer, component string) error {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vantage_build_info",
		Help: "Always 1; the labels say which build of which component is running.",
		ConstLabels: prometheus.Labels{
			"component": component,
			"version":   Version,
			"revision":  Rev(),
			"goversion": runtime.Version(),
		},
	})
	g.Set(1)
	if err := reg.Register(g); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			return nil
		}
		return err
	}
	return nil
}
