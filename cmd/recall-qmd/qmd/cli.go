package qmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
)

// Result is one finished qmd invocation.
//
// A non-zero exit code is data rather than an error, exactly as it is for the
// td adapter: qmd exits 0 for an empty result set and for "collection not
// found" alike, so its exit status carries no outcome semantics at all and
// every classification in this package is made from the output. Only an
// invocation that could not be run, or one whose context expired, comes back
// as an error.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Elapsed  time.Duration
}

// Runner executes one qmd invocation.
//
// It is an interface so a fixture can stand in for the executable. Nothing
// below this line knows whether a process was involved, which is what lets the
// parser, the candidate construction, and every failure path be exercised
// without the binary, without models, and without a corpus.
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)

	// Kind names the transport for diagnostics: "live" or "replay".
	Kind() string

	// Now reports a fixture-stated clock. A live runner reports false and the
	// adapter reads the wall clock; a recording states its own time so a
	// committed transcript does not carry the recording machine's clock.
	Now() (time.Time, bool)

	// Root reports the directory a replayed corpus lives in, so a recorded
	// `qmd collection show` can name a portable path instead of the absolute
	// one of the machine that recorded it. A live runner reports false.
	Root() (string, bool)
}

// ExecRunner runs the real qmd executable.
type ExecRunner struct {
	// Binary is the executable path, or a name resolved on PATH.
	Binary string

	// Timeout bounds one invocation. It composes with the caller's deadline
	// rather than replacing it: whichever is sooner wins, so a request budget
	// is never overrun by a source that decided its own.
	Timeout time.Duration

	// Dir is the working directory every invocation runs in, and it is the
	// corpus location.
	//
	// It is not cosmetic. qmd resolves its index by walking UP from the working
	// directory looking for a project-local `.qmd`, falling back to the shared
	// one in its cache. Inheriting Recall's working directory would therefore
	// make one configuration search different indexes depending on where Recall
	// was started — the same defect as resolving a settings path against the
	// process working directory, with a whole corpus behind it. Anchoring on the
	// corpus makes the resolution a property of the configuration, and it is
	// also what lets a corpus carry its own index.
	Dir string

	// Env replaces the child environment when non-nil, matching os/exec. A nil
	// value inherits, which is what qmd needs to find its own cache directory.
	Env []string
}

func (r ExecRunner) Kind() string           { return "live" }
func (r ExecRunner) Now() (time.Time, bool) { return time.Time{}, false }
func (r ExecRunner) Root() (string, bool)   { return "", false }

// Run spawns qmd and waits for it.
func (r ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, r.Binary, args...) //nolint:gosec // binary is explicit user configuration; argv passed checkAllowed
	cmd.Env = r.Env
	cmd.Dir = r.Dir
	// Never inherited: qmd prompts for nothing, and a child holding the
	// adapter's stdin would be reading the protocol stream.
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Elapsed: time.Since(start)}

	switch {
	case err == nil:
		return res, nil

	case ctx.Err() != nil:
		// A killed child also exits non-zero. Reporting its exit status here
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
	// ever asked of the index, so this is unreachable rather than failed — and
	// it is the shape a missing qmd install takes, which must degrade coverage
	// with this source named rather than look like an empty corpus.
	return res, fmt.Errorf("%w: cannot run %s: %w", protocol.ErrSourceUnavailable, r.Binary, err)
}

var _ Runner = ExecRunner{}

// Argument templates: the complete set of argv shapes this adapter may spawn.
//
// This is an allowlist of whole invocations rather than of subcommands, which
// is stricter than the td adapter's rule and for a reason qmd forces. td's
// flags accept a joined value, so "flags must be written as --flag=value" is
// enough to keep a flag's value from being read as a positional argument.
// qmd's do not: `-n 5` and `-c name` are two tokens each. A generic rule
// cannot tell those values from operands, so the shapes are written out
// instead and matched positionally.
//
// What that buys is worth the verbosity. `qmd cleanup`, `qmd collection
// remove`, `qmd init`, and `qmd mcp` are not merely undeclared, they are
// unreachable: there is no argv this package can build that reaches them.
// Free query text occupies exactly one position, last, behind `--`, so no
// query can be parsed as a flag or as a subcommand however it begins.
var allowedArgv = []argvTemplate{
	{tokens: []string{"--version"}},
	{tokens: []string{"status"}},
	{tokens: []string{"collection", "show", holeName}},

	{tokens: []string{"search", "--json", "-n", holeCount, "-c", holeName, "--", holeText}},
	{tokens: []string{"vsearch", "--json", "-n", holeCount, "-c", holeName, "--", holeText}},
	{tokens: []string{"query", "--json", "--explain", "--no-rerank", "-n", holeCount, "-c", holeName, "--", holeText}},
	{tokens: []string{"query", "--json", "--explain", "-n", holeCount, "-c", holeName, "--", holeText}},

	// Index maintenance. Reachable only through recall/refresh: [Adapter.run]
	// refuses a maintenance shape, so no search or expansion can rebuild an
	// index as a side effect of a query.
	{tokens: []string{"update"}, maintenance: true},
	{tokens: []string{"embed", "-c", holeName}, maintenance: true},
}

// Typed holes in an argv template.
const (
	holeName  = "\x00name"
	holeCount = "\x00count"
	holeText  = "\x00text"
)

type argvTemplate struct {
	tokens      []string
	maintenance bool
}

var (
	namePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	countPattern = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// ErrNotAllowed reports an attempt to spawn something outside [allowedArgv].
// It is a defect in this package, never something a query can cause, so it is
// distinct from every source failure.
var ErrNotAllowed = errors.New("qmd: refusing an invocation outside the allowlist")

// checkAllowed is the last gate before a process is spawned. maintenance says
// whether the caller is allowed to reach an index-mutating shape.
func checkAllowed(args []string, maintenance bool) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no subcommand", ErrNotAllowed)
	}
	for _, tmpl := range allowedArgv {
		if !tmpl.matches(args) {
			continue
		}
		if tmpl.maintenance && !maintenance {
			return fmt.Errorf("%w: %q rebuilds the index and is reachable only from a refresh",
				ErrNotAllowed, args[0])
		}
		return nil
	}
	return fmt.Errorf("%w: %q", ErrNotAllowed, strings.Join(args, " "))
}

func (t argvTemplate) matches(args []string) bool {
	if len(args) != len(t.tokens) {
		return false
	}
	for i, want := range t.tokens {
		got := args[i]
		switch want {
		case holeName:
			if !namePattern.MatchString(got) {
				return false
			}
		case holeCount:
			if !countPattern.MatchString(got) {
				return false
			}
		case holeText:
			// Free text. It is the final token and it sits behind `--`, so its
			// content cannot change how anything before it is parsed. An empty
			// query never reaches here: [Adapter.Search] declines one.
			if got == "" {
				return false
			}
		default:
			if got != want {
				return false
			}
		}
	}
	return true
}

// errBrokenContract reports output qmd should not have produced.
//
// It is separate from unreachable because the two are different facts about
// the source. The cold-start hazard is the case this exists for: the first qmd
// run after an install or a model eviction writes a spinner and download
// progress to STDOUT, ahead of or instead of the JSON document `--json`
// promises. Reading that as "no results" would turn a 2GB model download into
// a successful empty search, which is the false absence this whole system is
// built to prevent. Model warm-up therefore belongs to recall/refresh, and
// anything on stdout that is not the promised JSON array is reported here.
var errBrokenContract = errors.New("qmd: invalid_response")

// decodeResults reads a qmd result array off a finished invocation.
//
// The failure modes are kept apart deliberately. A non-zero exit means qmd
// could not be run against the index and is unreachable. Output that does not
// parse as a JSON array is a broken contract. Neither may become an empty
// success, which is what returning `nil, nil` here would produce — and `[]`,
// which qmd really does write for a query that matched nothing, is the one
// empty answer this function may return.
func decodeResults(res Result, args ...string) ([]searchHit, error) {
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%w: qmd %s exited %d: %s",
			protocol.ErrSourceUnavailable, args[0], res.ExitCode, safeDetail(res.Stderr, res.Stdout))
	}
	body := bytes.TrimSpace(res.Stdout)
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: qmd %s wrote nothing to stdout; `[]` is how an empty result is spelled",
			errBrokenContract, args[0])
	}
	if body[0] != '[' {
		// Cold-start progress output, a usage dump, or a crash banner. Naming
		// the first line is what makes a 2GB download recognizable in a
		// diagnostic; it is scrubbed first because qmd colorizes it.
		return nil, fmt.Errorf("%w: qmd %s wrote non-JSON output on stdout: %s",
			errBrokenContract, args[0], safeDetail(res.Stdout))
	}
	var out []searchHit
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: qmd %s wrote unreadable JSON on stdout", errBrokenContract, args[0])
	}
	return out, nil
}

// decodeText reads a human-readable qmd report off a finished invocation.
//
// `qmd status`, `qmd collection show`, and `qmd --version` have no JSON form in
// qmd 2.5.3: `--format json` is accepted and ignored by all three. Their text
// is therefore parsed, and every parse in [status.go] treats a field it cannot
// find as unknown rather than as a default — a missing count must degrade
// coverage, never assert a complete one.
func decodeText(res Result, args ...string) (string, error) {
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w: qmd %s exited %d: %s",
			protocol.ErrSourceUnavailable, args[0], res.ExitCode, safeDetail(res.Stderr, res.Stdout))
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		return "", fmt.Errorf("%w: qmd %s wrote nothing to stdout", errBrokenContract, args[0])
	}
	return string(res.Stdout), nil
}

// safeDetailLimit bounds how much CLI output is quoted into an error. The
// message travels into diagnostics a person reads; a progress bar does not.
const safeDetailLimit = 200

// safeDetail quotes the first useful line of a stream, scrubbed.
//
// Scrubbing is not cosmetic. qmd colorizes and animates its progress output,
// so this text arrives carrying terminal escape sequences and carriage
// returns, and docs/spec.md requires control characters to be stripped before
// anything from a source reaches a terminal. Diagnostics are read by a person
// in exactly that terminal.
func safeDetail(streams ...[]byte) string {
	for _, s := range streams {
		for line := range bytes.Lines(s) {
			clean := scrub(string(line))
			if clean == "" {
				continue
			}
			if len(clean) > safeDetailLimit {
				return clean[:cutRunes(clean, safeDetailLimit)] + "…"
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

// cutRunes returns the largest index at or below limit that starts a rune, so
// a quoted diagnostic never ends in half a character.
func cutRunes(s string, limit int) int {
	if limit >= len(s) {
		return len(s)
	}
	for limit > 0 && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return limit
}
