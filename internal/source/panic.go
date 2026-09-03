package source

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// panicLog is where a recovered panic's stack is written. It is a variable so a
// test can read what a crash reported instead of racing the process's stderr.
var panicLog io.Writer = os.Stderr

// RecoveredPanic records a panic caught on a per-source goroutine and returns
// the one line that names it in a source report.
//
// The stack goes to standard error and nowhere else. Every surface recall has
// puts its answer on standard output — the CLI's JSON, the API's body, the
// Sidecar plugin's single response object — so a stack written anywhere else
// would corrupt the very answer this recovery exists to keep deliverable.
//
// It lives here rather than in each caller because a panic on a fan-out
// goroutine is the same event wherever it happens: one source crashed, its
// absence degrades coverage, and the other sources' answers are still owed to
// the caller. See [ReasonPanicked].
func RecoveredPanic(where string, value any) string {
	detail := fmt.Sprintf("%v", value)
	_, _ = fmt.Fprintf(panicLog, "recall: %s panicked: %s\n%s\n", where, detail, debug.Stack())
	return detail
}
