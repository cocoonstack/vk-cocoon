// Package version holds build-time metadata injected via ldflags.
package version

// Build-time metadata injected via -ldflags.
var (
	REVISION = "unknown"
	VERSION  = "dev"
	BUILTAT  = "unknown"
)
