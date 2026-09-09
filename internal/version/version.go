// Package version holds the build identity that `fivepanel-agent version`
// prints and that the hello message carries. The values are overwritten at
// build time with -ldflags "-X github.com/5panel/agent/internal/version.Version=…".
package version

import "runtime"

// Set by the linker at release time; the defaults describe a local build.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// GoVersion is the toolchain the binary was compiled with.
func GoVersion() string { return runtime.Version() }
