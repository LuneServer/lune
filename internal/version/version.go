package version

// Build metadata injected at link time via -ldflags.
var (
	Version   = "0.1.0-dev"
	Commit    = "none"
	BuildDate = "unknown"
)
