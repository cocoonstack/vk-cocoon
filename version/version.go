// Package version holds build-time metadata injected via ldflags.
package version

// Set by goreleaser / Makefile ldflags at build time.
var (
	REVISION = "unknown"
	VERSION  = "dev"
	BUILTAT  = "unknown"
)
