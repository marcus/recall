package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
)

// Result is one finished Tasks CLI invocation.
//
// A non-zero ExitCode is data, not an error: `show` exits 2 for "no such
// task", which is a perfectly normal answer. Only a command that could not be
// run at all, or one whose context expired, comes back as an error.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Elapsed  time.Duration
}

// Runner executes one read-only Tasks CLI invocation.
//
// It is an interface so tests can supply a fake CLI. Nothing below this line
// knows whether a process was involved, which is what lets the parser, the
// ranking, and every failure path be exercised without the real binary.
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// ExecRunner runs the real executable.
type ExecRunner struct {
	// Binary is the executable path, or a name resolved on PATH.
	Binary string

	// Env replaces the child environment when non-nil, matching os/exec. The
	// adapter uses it to point the CLI at the configured store.
	Env []string

	// Dir is the child working directory. Empty inherits.
	Dir string
}

// Run spawns the CLI and waits for it.
func (r ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Env = r.Env
	cmd.Dir = r.Dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Elapsed: time.Since(start)}

	switch {
	case err == nil:
		return res, nil

	case ctx.Err() != nil:
		// A killed child also exits non-zero. Reporting the exit status here
		// would turn a timeout into a source failure and lose the distinction
		// the core reports as `timeout` rather than `unavailable`.
		return res, ctx.Err()
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	// The binary is absent, is not executable, or the fork failed. Nothing was
	// ever asked of the source, so this is unreachable rather than failed.
	return res, fmt.Errorf("%w: cannot run %s: %w", protocol.ErrSourceUnavailable, r.Binary, err)
}

// readOnlyCommands is the complete set of Tasks subcommands this adapter may
// invoke. It is an allowlist rather than a denylist of mutations because the
// CLI grows commands and a denylist would silently admit each new one.
//
// `id` is deliberately absent even though it only prints: it mints a stable id
// when the record has none, so it writes.
var readOnlyCommands = map[string]struct{}{
	"list":     {},
	"show":     {},
	"projects": {},
	"check":    {},
	"config":   {},
	"links":    {},
}

// ErrNotReadOnly reports an attempt to invoke something outside the allowlist.
// It is a defect in this package, never something a query can cause, so it is
// distinct from every source failure.
var ErrNotReadOnly = errors.New("tasks: refusing a non-read-only invocation")

// checkReadOnly is the last gate before a process is spawned.
//
// Beyond the subcommand allowlist it constrains every following argument to a
// flag, a `/text` filter, or a stable id. User query text reaches the CLI only
// inside a `/text` filter, so no query can be parsed as a subcommand or as a
// positional reference, and retrieved content never shapes an invocation.
func checkReadOnly(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no subcommand", ErrNotReadOnly)
	}
	if _, ok := readOnlyCommands[args[0]]; !ok {
		return fmt.Errorf("%w: %q", ErrNotReadOnly, args[0])
	}
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "-"), strings.HasPrefix(arg, "/"):
			continue
		case idPattern.MatchString(arg):
			continue
		default:
			return fmt.Errorf("%w: bare argument %q", ErrNotReadOnly, arg)
		}
	}
	return nil
}

// run invokes the CLI under the adapter's per-invocation timeout.
//
// The timeout composes with the caller's deadline rather than replacing it:
// whichever is sooner wins, so a request budget is never overrun by a source
// that decided its own.
func (a *Adapter) run(ctx context.Context, args ...string) (Result, error) {
	if err := checkReadOnly(args); err != nil {
		return Result{}, err
	}
	runner, timeout, err := a.session()
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runner.Run(ctx, args...)
}

// decodeJSON turns a finished invocation into a parsed value.
//
// The two failure modes are kept apart on purpose. A non-zero exit means the
// store could not be read and is reported as unreachable; output that is not
// JSON means the contract broke and is reported as a plain failure. Neither
// may ever become an empty success, which is what an adapter returning
// `nil, nil` here would produce.
func decodeJSON(res Result, v any, args ...string) error {
	if res.ExitCode != 0 {
		return fmt.Errorf("%w: tasks %s exited %d: %s",
			protocol.ErrSourceUnavailable, args[0], res.ExitCode, safeDetail(res.Stderr, res.Stdout))
	}
	if err := json.Unmarshal(res.Stdout, v); err != nil {
		return fmt.Errorf("tasks %s: unreadable JSON output: %w", args[0], err)
	}
	return nil
}

// safeDetailLimit bounds how much CLI output is quoted into an error. The
// message travels into diagnostics a person reads; a Ruby backtrace does not.
const safeDetailLimit = 200

func safeDetail(streams ...[]byte) string {
	for _, s := range streams {
		line, _, _ := bytes.Cut(bytes.TrimSpace(s), []byte("\n"))
		if len(line) == 0 {
			continue
		}
		if len(line) > safeDetailLimit {
			return string(line[:safeDetailLimit]) + "…"
		}
		return string(line)
	}
	return "no output"
}
