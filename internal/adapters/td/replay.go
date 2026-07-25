package td

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ReplayFile is the name of the argv-to-output map inside a replay directory.
const ReplayFile = "td.json"

// ReplayRunner answers td invocations from recorded output instead of running
// the executable.
//
// A committed evaluation pack cannot spawn the real td: the binary may not be
// installed, and a workspace changes with every commit, so a pack built on one
// would measure something different every run. Recording td's real output once
// and replaying it keeps the adapter under test — the parser, the identifier
// matching, the merge — while making the workspace it reads a fixture.
//
// It is configured rather than injected because a pack is configuration. An
// adapter that could only be made deterministic from Go would leave a
// committed pack unable to exercise it at all.
type ReplayRunner struct {
	dir   string
	rules replayRules
}

type replayRules struct {
	// Comment is prose in the recording, carried so a round trip keeps it.
	Comment any `json:"comment,omitempty"`
	// Invocations are tried in order, so a specific match can precede a
	// general one.
	Invocations []replayRule `json:"invocations"`
	// Default answers anything no invocation matched.
	Default *replayResponse `json:"default"`
}

type replayRule struct {
	// Args matches an invocation's argv exactly.
	Args []string `json:"args,omitempty"`
	// Contains matches any invocation holding all of these arguments.
	Contains []string `json:"contains,omitempty"`

	replayResponse
}

type replayResponse struct {
	// Stdout names a recorded payload file in the replay directory. It is a
	// file name rather than inline text because the recordings are real td
	// output, and inlining them would make the map unreadable and the
	// recording something a person edits by hand instead of captures.
	Stdout string `json:"stdout,omitempty"`
	// Stderr is inline, because what td writes there is a line or two.
	Stderr string `json:"stderr,omitempty"`
	// ExitCode records what td returned. A miss answered with exit 1 and a
	// not_found envelope is what the real CLI does, and a fixture that
	// answered 0 instead would make the adapter's error handling untestable.
	ExitCode int `json:"exit_code,omitempty"`

	// DelayMS makes a recorded invocation slow.
	//
	// It exists because two required behaviors cannot be recorded from an
	// instant fixture: cancelling a request that is still in flight, and
	// enforcing a deadline. A recording that always answered immediately would
	// leave both untestable against anything but the real binary, which is the
	// thing a fixture is there to avoid needing.
	DelayMS int `json:"delay_ms,omitempty"`
}

// NewReplayRunner reads a replay directory.
func NewReplayRunner(dir string) (*ReplayRunner, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ReplayFile))
	if err != nil {
		return nil, fmt.Errorf("td replay: %w", err)
	}
	var rules replayRules
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("td replay %s: %w", ReplayFile, err)
	}
	if len(rules.Invocations) == 0 && rules.Default == nil {
		return nil, fmt.Errorf("td replay %s: no rules and no default", ReplayFile)
	}
	return &ReplayRunner{dir: dir, rules: rules}, nil
}

// Run answers one invocation from the recording.
func (r *ReplayRunner) Run(ctx context.Context, args ...string) (Result, error) {
	for _, rule := range r.rules.Invocations {
		if rule.matches(args) {
			return r.respond(ctx, rule.replayResponse)
		}
	}
	if r.rules.Default != nil {
		return r.respond(ctx, *r.rules.Default)
	}
	// An unrecorded invocation is a gap in the fixture, and saying so is more
	// useful than an empty result the adapter would report as "no matches".
	return Result{ExitCode: 1, Stderr: []byte("no recorded response")},
		fmt.Errorf("td replay: no recorded response for %v", args)
}

func (rule replayRule) matches(args []string) bool {
	if len(rule.Args) > 0 {
		return slices.Equal(rule.Args, args)
	}
	if len(rule.Contains) == 0 {
		return false
	}
	for _, want := range rule.Contains {
		if !slices.Contains(args, want) {
			return false
		}
	}
	return true
}

func (r *ReplayRunner) respond(ctx context.Context, resp replayResponse) (Result, error) {
	if resp.DelayMS > 0 {
		// Interruptible, exactly as a real child process is: a cancelled or
		// expired request must not wait out the recorded delay, or the
		// recording would be testing the harness's patience rather than the
		// adapter's deadline handling.
		timer := time.NewTimer(time.Duration(resp.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	out := Result{
		Stderr:   []byte(resp.Stderr),
		ExitCode: resp.ExitCode,
		// Fixed, not measured: a replayed run reports no wall time of its own,
		// and a timing that varied would make an evaluation run
		// irreproducible.
		Elapsed: 0,
	}
	if resp.Stdout != "" {
		// Contained to the replay directory: a fixture naming ../../etc would
		// be reading something the pack does not ship.
		clean := filepath.Clean(resp.Stdout)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return Result{}, fmt.Errorf("td replay: %q escapes the replay directory", resp.Stdout)
		}
		body, err := os.ReadFile(filepath.Join(r.dir, clean))
		if err != nil {
			return Result{}, fmt.Errorf("td replay: %w", err)
		}
		out.Stdout = body
	}
	// A non-zero exit is data, not an error, exactly as ExecRunner reports it:
	// the caller reads ExitCode. Returning an error here would make a recorded
	// "no such issue" look like a source failure.
	return out, nil
}

var _ Runner = (*ReplayRunner)(nil)
