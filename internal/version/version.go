// Package version reports which build of the auto-agent binary is running.
package version

import (
	"fmt"
	"runtime/debug"
)

// Set at build time:
//
//	go build -ldflags "-X github.com/benjr70/auto-agent/internal/version.version=v1.2.3 \
//	  -X github.com/benjr70/auto-agent/internal/version.commit=abc1234" ./cmd/auto-agent
var (
	version string
	commit  string
)

// Info names one build of the binary. An empty field means it is not known.
type Info struct {
	Version string
	Commit  string
}

// Current is the running binary's build: the values injected at build time,
// and for whatever was not injected, what the Go toolchain stamped into the
// binary (the module version under `go install`, the revision in a checkout).
func Current() Info {
	info := Info{Version: version, Commit: commit}
	if info.Version != "" && info.Commit != "" {
		return info
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if info.Version == "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	if info.Commit == "" {
		for _, s := range build.Settings {
			if s.Key == "vcs.revision" {
				info.Commit = s.Value
			}
		}
	}
	return info
}

// String is the line `auto-agent version` prints. A build nothing is known
// about is a development build.
func (i Info) String() string {
	v, c := i.Version, i.Commit
	if v == "" {
		v = "dev"
	}
	if c == "" {
		c = "unknown"
	}
	return fmt.Sprintf("auto-agent %s (commit %s)", v, c)
}
