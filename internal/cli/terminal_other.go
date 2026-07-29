//go:build !darwin && !linux

package cli

import "os"

// Interactive prompting is deliberately unavailable on platforms without a
// local terminal probe. The deterministic --docs path remains fully supported.
func isTerminal(_ *os.File) bool { return false }
