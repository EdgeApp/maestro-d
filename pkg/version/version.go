// Package version identifies the build of the maestro-d binaries.
package version

// Build info — set at build time via -ldflags.
var (
	Version   = "3.4.0"
	Commit    = "unknown"
	BuildDate = "unknown"
)
