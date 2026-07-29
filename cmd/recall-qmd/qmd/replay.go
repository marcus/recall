package qmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/protocol"
)

// ReplayFile is the name of the argv-to-output map inside a replay directory.
const ReplayFile = "qmd.json"

// ReplayClockFile optionally states the time a recording was made at, so a
// transcript recorded from it carries the fixture's clock instead of the
// recording machine's.
const ReplayClockFile = "clock.json"

// ReplayCorpusDir is the directory inside a replay pack that holds the corpus
// the recorded results were produced from.
//
// It has to exist because expansion does not go through qmd: a locator names a
// file and a line range, and evidence is read from the file. A pack shipping
// recorded search output but no corpus could exercise search and nothing else.
const ReplayCorpusDir = "corpus"

// ReplayRootToken is substituted for the replay directory's absolute path in
// recorded stdout.
//
// It exists for one specific value. `qmd collection show` reports the
// directory the collection indexes, and this adapter compares that against its
// configured location on every operation, because a collection pointing
// somewhere else means every candidate names a file from a corpus the source
// was not configured for. A recorded report therefore has to contain a path —
// and a recording that contained the absolute path of the machine it was made
// on would both leak a home directory into a committed fixture and fail the
// comparison everywhere else. The token keeps the check real and the fixture
// portable.
const ReplayRootToken = "${REPLAY_DIR}"

// ReplayRunner answers qmd invocations from recorded output instead of running
// the executable.
//
// A committed evaluation pack cannot spawn the real qmd: the binary may not be
// installed, it needs about 2GB of model files, and its query-expansion layer
// is an LLM, so a pack built on the live tool would measure something slightly
// different every run. Recording qmd's real output once and replaying it keeps
// this adapter under test — the parser, the identity derivation, the relevance
// recomputation, the coverage rules — while making the index it reads a
// fixture.
//
// It is configured through the `replay` settings key rather than injected from
// Go, because a pack is configuration. An adapter that could only be made
// deterministic from a test would leave a committed pack unable to exercise it
// at all.
type ReplayRunner struct {
	dir   string
	rules replayRules
	now   time.Time
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
	// file name rather than inline text because the recordings are real qmd
	// output, and inlining them would make the map unreadable and the recording
	// something a person edits by hand instead of captures.
	Stdout string `json:"stdout,omitempty"`
	// Stderr is inline, because what qmd writes there is a line or two of
	// progress. It is never parsed.
	Stderr string `json:"stderr,omitempty"`
	// ExitCode records what qmd returned. qmd exits 0 for an empty result and
	// for "collection not found" alike, so a fixture that always answered 0 is
	// realistic; a non-zero recording is how a broken install is replayed.
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
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd replay: %v", err)
	}
	abs = filepath.Clean(abs)
	raw, err := os.ReadFile(filepath.Join(abs, ReplayFile))
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd replay: cannot read %s in the configured replay directory", ReplayFile)
	}
	var rules replayRules
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd replay %s: %v", ReplayFile, err)
	}
	if len(rules.Invocations) == 0 && rules.Default == nil {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd replay %s: no rules and no default", ReplayFile)
	}
	r := &ReplayRunner{dir: abs, rules: rules}
	if clock, err := os.ReadFile(filepath.Join(abs, ReplayClockFile)); err == nil {
		var stated struct {
			Now time.Time `json:"now"`
		}
		if json.Unmarshal(clock, &stated) == nil {
			r.now = stated.Now.UTC()
		}
	}
	return r, nil
}

func (r *ReplayRunner) Kind() string { return "replay" }

func (r *ReplayRunner) Now() (time.Time, bool) { return r.now, !r.now.IsZero() }

// Root is the replay pack's corpus directory, which is what a recorded
// `collection show` names through [ReplayRootToken].
func (r *ReplayRunner) Root() (string, bool) {
	return filepath.Join(r.dir, ReplayCorpusDir), true
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
		fmt.Errorf("%w: qmd replay: no recorded response for %v",
			protocol.ErrSourceUnavailable, args)
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
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	out := Result{
		Stderr:   []byte(resp.Stderr),
		ExitCode: resp.ExitCode,
		// Fixed, not measured: a replayed run reports no wall time of its own,
		// and a timing that varied would make a recorded transcript and an
		// evaluation run irreproducible.
		Elapsed: 0,
	}
	if resp.Stdout != "" {
		// Contained to the replay directory: a fixture naming ../../etc would
		// be reading something the pack does not ship.
		clean := filepath.Clean(resp.Stdout)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return Result{}, fmt.Errorf("qmd replay: %q escapes the replay directory", resp.Stdout)
		}
		body, err := os.ReadFile(filepath.Join(r.dir, clean))
		if err != nil {
			return Result{}, fmt.Errorf("qmd replay: cannot read recorded stdout %q", clean)
		}
		out.Stdout = []byte(strings.ReplaceAll(string(body), ReplayRootToken, r.dir))
	}
	// A non-zero exit is data, not an error, exactly as ExecRunner reports it:
	// the caller reads ExitCode. Returning an error here would make a recorded
	// "collection not found" look like a failure to run qmd.
	return out, nil
}

var _ Runner = (*ReplayRunner)(nil)
