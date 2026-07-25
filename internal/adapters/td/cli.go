package td

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/marcus/recall/internal/protocol"
)

// Result is one finished td invocation.
//
// A non-zero ExitCode is data, not an error: `td show` exits 1 with a
// not_found envelope for an id the workspace does not hold, which is a normal
// answer. Only a command that could not be run at all, or one whose context
// expired, comes back as an error.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Elapsed  time.Duration
}

// Runner executes one read-only td invocation.
//
// It is an interface so tests can supply a fake CLI. Nothing below this line
// knows whether a process was involved, which is what lets the parser, the
// merge, and every failure path be exercised without the real binary and
// without a real workspace.
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// PinnedRunner can run a command with td's supported --work-dir flag pointed
// at an already-resolved database root. An adapter runner that cannot do this
// cannot safely serve live evidence: separate commands against the original
// location can resolve different associations.
type PinnedRunner interface {
	RunPinned(ctx context.Context, root string, args ...string) (Result, error)
}

// ExecRunner runs the real executable against one workspace.
type ExecRunner struct {
	// Binary is the executable path, or a name resolved on PATH.
	Binary string

	// Root is the workspace directory. It is passed as td's global
	// `--work-dir`, which is how td is told which database to resolve: td
	// walks from there to `.td-root`, the git root, or the main worktree. That
	// resolution is td's, deliberately — duplicating it here would mean
	// disagreeing with it on worktrees and submodules.
	Root string

	// Env replaces the child environment when non-nil, matching os/exec.
	Env []string
}

// Run spawns td and waits for it.
func (r ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	return r.runAt(ctx, r.Root, args...)
}

// RunPinned overrides the configured location with one resolved store root.
func (r ExecRunner) RunPinned(ctx context.Context, root string, args ...string) (Result, error) {
	return r.runAt(ctx, root, args...)
}

func (r ExecRunner) runAt(ctx context.Context, root string, args ...string) (Result, error) {
	start := time.Now()
	full := args
	if root != "" {
		// Prepended, not appended: it is a global flag, and putting it before
		// the subcommand keeps it out of the argument the subcommand parses.
		full = append([]string{"--work-dir=" + root}, args...)
	}
	cmd := exec.CommandContext(ctx, r.Binary, full...)
	cmd.Env = r.Env

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
	// ever asked of the workspace, so this is unreachable rather than failed.
	return res, fmt.Errorf("%w: cannot run %s: %w", protocol.ErrSourceUnavailable, r.Binary, err)
}

// readOnlyCommands is the complete set of td subcommands this adapter may
// invoke. It is an allowlist rather than a denylist of mutations because td
// grows commands — it has more than sixty — and a denylist would silently
// admit each new one.
//
// `serve` is deliberately absent even though it only reads issues: it starts a
// long-lived process that accepts writes, and an adapter that started one
// would be leaving something running behind a search.
var readOnlyCommands = map[string]struct{}{
	"search":     {},
	"list":       {},
	"show":       {},
	"info":       {},
	"files":      {},
	"depends-on": {},
	"blocked-by": {},
}

// ErrNotReadOnly reports an attempt to invoke something outside the allowlist.
// It is a defect in this package, never something a query can cause, so it is
// distinct from every source failure.
var ErrNotReadOnly = errors.New("td: refusing a non-read-only invocation")

// checkReadOnly is the last gate before a process is spawned.
//
// Beyond the subcommand allowlist it constrains the argument list itself.
// Flags must be written in joined form (`--limit=50`), so a flag's value can
// never be mistaken for a positional argument. Free text — the only place user
// query text is allowed — must come last and behind a `--` separator, so no
// query can be parsed as a flag or as a subcommand however it begins. Every
// other positional argument must be a td issue id.
func checkReadOnly(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no subcommand", ErrNotReadOnly)
	}
	sub := args[0]
	if _, ok := readOnlyCommands[sub]; !ok {
		return fmt.Errorf("%w: %q", ErrNotReadOnly, sub)
	}
	for i, arg := range args[1:] {
		switch {
		case arg == "--":
			// Everything after the separator is positional. Exactly one value,
			// and only `search` may carry free text; every other command takes
			// ids, which are checked below like any bare argument.
			tail := args[i+2:]
			if len(tail) != 1 {
				return fmt.Errorf("%w: %d positional arguments after --, want 1", ErrNotReadOnly, len(tail))
			}
			if sub != "search" && !idPattern.MatchString(tail[0]) {
				return fmt.Errorf("%w: %q is free text for a subcommand that takes ids", ErrNotReadOnly, sub)
			}
			return nil
		case strings.HasPrefix(arg, "-"):
			// Joined form only: a separated value would be a bare argument on
			// the next iteration, and this loop cannot tell it from a
			// positional one.
			if !strings.Contains(arg, "=") && !isBoolFlag(arg) {
				return fmt.Errorf("%w: flag %q must carry its value joined, as --flag=value", ErrNotReadOnly, arg)
			}
		case idPattern.MatchString(arg):
		default:
			return fmt.Errorf("%w: bare argument %q", ErrNotReadOnly, arg)
		}
	}
	return nil
}

// isBoolFlag names the valueless flags this package emits. Keeping the set
// explicit is what lets [checkReadOnly] reject every other separated value
// without having to know td's whole flag table.
func isBoolFlag(arg string) bool {
	switch arg {
	case "--json", "--all":
		return true
	default:
		return false
	}
}

// run invokes td under the adapter's per-invocation timeout.
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
	if root, ok := ctx.Value(pinnedRootKey{}).(string); ok {
		pinned, ok := runner.(PinnedRunner)
		if !ok {
			return Result{}, fmt.Errorf("%w: td runner cannot pin --work-dir to a verified store",
				protocol.ErrSourceUnavailable)
		}
		return pinned.RunPinned(ctx, root, args...)
	}
	return runner.Run(ctx, args...)
}

type pinnedRootKey struct{}

func withPinnedRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, pinnedRootKey{}, root)
}

// errNotFound reports that td resolved the workspace and does not hold the
// record asked for. It is an answer about the source, not a failure of it,
// which is why it is a distinct sentinel rather than one more unavailable.
var errNotFound = errors.New("td: no such record")

// envelope is td's CLI error shape, `{"error":{"code":..,"message":..}}`.
type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeJSON turns a finished invocation into a parsed value.
//
// The failure modes are kept apart on purpose. `not_found` means the workspace
// answered and holds no such record. Any other non-zero exit means the
// workspace could not be read — a missing database reports
// `database not found: run 'td init' first` here — and is reported as
// unreachable. Output that is not JSON means the contract broke and is
// reported as a plain failure. None of them may become an empty success, which
// is what an adapter returning `nil, nil` here would produce.
func decodeJSON(res Result, v any, args ...string) error {
	if res.ExitCode != 0 {
		code, message := failure(res.Stdout, res.Stderr)
		if code == "not_found" {
			return fmt.Errorf("%w: td %s: %s", errNotFound, args[0], message)
		}
		return fmt.Errorf("%w: td %s exited %d: %s",
			protocol.ErrSourceUnavailable, args[0], res.ExitCode, message)
	}
	if err := json.Unmarshal(res.Stdout, v); err != nil {
		return fmt.Errorf("td %s: unreadable JSON output: %w", args[0], err)
	}
	return nil
}

// failure reads td's error envelope off a failed invocation.
//
// It scans stdout line by line for the FIRST line that parses as an envelope,
// which is neither the first line nor the whole document. A failed td command
// writes a colorized `ERROR:` banner ahead of its JSON, and a failed `td show`
// writes two envelopes — the specific `not_found` first and a generic
// `invalid_input` restatement after. Parsing the whole stream fails on the
// banner; taking the last value loses the reason.
func failure(stdout, stderr []byte) (code, message string) {
	for line := range bytes.Lines(stdout) {
		var env envelope
		if err := json.Unmarshal(bytes.TrimSpace(line), &env); err != nil || env.Error.Code == "" {
			continue
		}
		return env.Error.Code, safeDetail([]byte(env.Error.Message))
	}
	return "", safeDetail(stderr, stdout)
}

// safeDetailLimit bounds how much CLI output is quoted into an error. The
// message travels into diagnostics a person reads; a usage dump does not.
const safeDetailLimit = 200

// safeDetail quotes the first useful line of a stream, scrubbed.
//
// Scrubbing is not cosmetic. td colorizes its errors, so this text arrives
// carrying terminal escape sequences, and docs/spec.md requires control
// characters to be stripped before anything from a source reaches a terminal.
// Diagnostics are read by a person in exactly that terminal.
func safeDetail(streams ...[]byte) string {
	for _, s := range streams {
		for line := range bytes.Lines(s) {
			clean := scrub(string(line))
			if clean == "" {
				continue
			}
			if len(clean) > safeDetailLimit {
				return clean[:safeDetailLimit] + "…"
			}
			return clean
		}
	}
	return "no output"
}

// scrub removes ANSI escape sequences and control characters from one line.
func scrub(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b {
			// Skip the whole escape sequence: everything up to and including
			// its final byte, which for CSI is the first alphabetic character.
			for i++; i < len(line); i++ {
				if (line[i] >= 'a' && line[i] <= 'z') || (line[i] >= 'A' && line[i] <= 'Z') {
					break
				}
			}
			continue
		}
		if c < 0x20 || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}
