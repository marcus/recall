package source

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
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

// RunGuarded runs one source's work on its own goroutine, writes what it
// produced into slot, and converts a panic in adapter code into the typed
// result crashed returns for it.
//
// Every place recall asks its sources anything fans out: planning handshakes
// and probes them at once, query searches them at once, refresh maintains them
// at once. Adapter code therefore runs on a goroutine the caller cannot recover
// on, and an unhandled panic there does not fail one source — it takes the
// process down along with every other source's answer and the response itself.
// Each surface recall has is a one-shot process that owes a single answer on
// standard output, so a dead process reads to the host as "recall crashed"
// rather than the truth, which is that one source did.
//
// This is the whole of that recovery, in one function, because the three
// fan-outs differ only in what a crashed source's result looks like — a failed
// search, an excluded plan verdict, a failed refresh — and never in the rule.
// Adding a fourth fan-out without it is how the pattern gets missed again.
//
// The guarantee stops here, at the per-source boundary. An adapter that starts
// its own goroutines is responsible for recovering on them: a panic there
// happens on a stack this function never sees, and no recover of ours can
// reach it.
//
// Two details are load-bearing. wg.Add runs on the caller's goroutine, before
// the work starts, so Wait cannot miss a source. And wg.Done is registered
// before the recovery, which means it runs after it and runs even if the
// recovery itself panics: a fan-out whose Wait never returns is a hang, which
// is worse than the crash it replaced.
func RunGuarded[T any](
	wg *sync.WaitGroup,
	slot *T,
	sourceID string,
	work func() T,
	crashed func(detail string) T,
) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if value := recover(); value != nil {
				*slot = crashed(RecoveredPanic("source "+sourceID, value))
			}
		}()
		*slot = work()
	}()
}
