package gmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
)

type Runner interface {
	Run(ctx context.Context, key string, args []string, operand string, out any) error
	Kind() string
	Now() (time.Time, bool)
}

var readOnlyCommands = [][]string{
	{"gmail", "search"},
	{"gmail", "thread", "get"},
	{"auth", "list"},
}

var alwaysArgs = []string{"-j", "--color=never", "--no-input", "--gmail-no-send"}

type liveRunner struct {
	binary  string
	account string
	timeout time.Duration
}

func (r *liveRunner) Kind() string           { return "live" }
func (r *liveRunner) Now() (time.Time, bool) { return time.Time{}, false }

func (r *liveRunner) argv(args []string, operand string) ([]string, error) {
	command := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			command = append(command, arg)
		}
	}
	allowed := false
	for _, prefix := range readOnlyCommands {
		if len(command) < len(prefix) {
			continue
		}
		match := true
		for i := range prefix {
			if command[i] != prefix[i] {
				match = false
				break
			}
		}
		if match {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: refusing gog subcommand outside the read-only whitelist")
	}
	argv := []string{"--account", r.account}
	argv = append(argv, alwaysArgs...)
	argv = append(argv, args...)
	if operand != "" {
		argv = append(argv, "--", operand)
	}
	return argv, nil
}

func (r *liveRunner) Run(ctx context.Context, key string, args []string, operand string, out any) error {
	argv, err := r.argv(args, operand)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.binary, argv...) //nolint:gosec // binary is explicit user configuration; argv is read-only
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return protocol.Errorf(protocol.CodeSourceUnavailable,
				"gmail: gog did not answer within %s", r.timeout)
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return classifyGog(exit.ExitCode(), stderr.String())
		}
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: could not run %s: %v", r.binary, err)
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: gog returned invalid JSON for %s", key)
	}
	return nil
}

type replayRunner struct {
	dir string
	now time.Time
}

func newReplayRunner(dir string) (*replayRunner, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"gmail: replay directory %q is not readable", dir)
	}
	r := &replayRunner{dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "clock.json"))
	if err == nil {
		var clock struct {
			Now time.Time `json:"now"`
		}
		if json.Unmarshal(raw, &clock) == nil {
			r.now = clock.Now.UTC()
		}
	}
	return r, nil
}

func (r *replayRunner) Kind() string { return "replay" }
func (r *replayRunner) Now() (time.Time, bool) {
	return r.now, !r.now.IsZero()
}

func (r *replayRunner) Run(ctx context.Context, key string, _ []string, _ string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(r.dir, key+".*.json"))
	if err != nil || len(matches) == 0 {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: no recorded gog response for %s", key)
	}
	sort.Strings(matches)
	path := matches[0]
	parts := strings.Split(filepath.Base(path), ".")
	if len(parts) < 3 {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: invalid recorded response name for %s", key)
	}
	exitCode, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: invalid recorded exit code for %s", key)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: cannot read recorded response for %s", key)
	}
	if exitCode != 0 {
		return classifyGog(exitCode, string(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: recorded response for %s is invalid JSON", key)
	}
	return nil
}

var deniedMarkers = []string{
	"no auth", "unauthorized", "invalid_grant", "invalid grant",
	"token expired", "refresh token", "permission denied",
	"insufficient permission", "403", "401",
}

var missingMarkers = []string{
	"not found", "notfound", "404", "no message", "no thread", "deleted",
}

func classifyGog(exitCode int, stderr string) error {
	lower := strings.ToLower(stderr)
	if exitCode == 4 || holdsAny(lower, deniedMarkers) {
		return protocol.Errorf(protocol.CodeSourceDenied,
			"gmail: gog has no usable credentials; run `gog auth doctor`")
	}
	first := sanitizeLine(strings.Split(strings.TrimSpace(stderr), "\n")[0])
	if first == "" {
		return protocol.Errorf(protocol.CodeSourceUnavailable,
			"gmail: gog exited %d", exitCode)
	}
	return protocol.Errorf(protocol.CodeSourceUnavailable,
		"gmail: gog exited %d: %s", exitCode, first)
}

func expiredIfMissing(err error, id string) error {
	lower := strings.ToLower(err.Error())
	if holdsAny(lower, missingMarkers) {
		return protocol.Errorf(protocol.CodeLocatorExpired,
			"gmail: thread %s is no longer in this mailbox", sanitizeLine(id))
	}
	return err
}

func holdsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isDenied(err error) bool {
	return errors.Is(err, protocol.ErrSourceDenied)
}

type authList struct {
	Accounts []struct {
		Email string `json:"email"`
	} `json:"accounts"`
}

type searchPayload struct {
	Threads       []thread `json:"threads"`
	NextPageToken string   `json:"nextPageToken"`
}

type thread struct {
	ID           string   `json:"id"`
	Date         string   `json:"date"`
	From         string   `json:"from"`
	Subject      string   `json:"subject"`
	Labels       []string `json:"labels"`
	MessageCount int      `json:"messageCount"`
}

type threadPayload struct {
	Thread struct {
		ID       string    `json:"id"`
		Messages []message `json:"messages"`
	} `json:"thread"`
}

type message struct {
	ID           string            `json:"id"`
	LabelIDs     []string          `json:"labelIds"`
	InternalDate json.Number       `json:"internalDate"`
	Headers      map[string]string `json:"headers"`
	Body         string            `json:"body"`
}

func (m message) lastValue() string {
	if m.InternalDate == "" {
		return "-"
	}
	return string(m.InternalDate)
}
