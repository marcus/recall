// Package buildinfo carries version identity stamped in at link time.
package buildinfo

// Version and Commit are set via -ldflags at build time. Evaluation runs
// record both, so they must never be empty in a released binary.
var (
	Version = "dev"
	Commit  = "unknown"
)
