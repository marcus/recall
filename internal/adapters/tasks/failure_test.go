package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// TestFailuresAreNeverEmptySuccess is invariant 2 from docs/spec.md, applied
// to every way this source can break.
//
// The invariant only bites when a failure is *plausible* as an empty result:
// a missing binary, a store that will not open, a wedged process, and output
// that is not JSON all produce zero candidates, and each one must be
// distinguishable from a store that simply holds no matching task. Fusion
// downstream cannot ask again — it sees only the outcome.
func TestFailuresAreNeverEmptySuccess(t *testing.T) {
	tests := []struct {
		name    string
		reply   func(args []string) (tasks.Result, error)
		outcome recall.SearchOutcome
		reason  string
		why     string
	}{
		{
			name: "CLI missing",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{}, fmt.Errorf("%w: cannot run tasks: %w",
					protocol.ErrSourceUnavailable, exec.ErrNotFound)
			},
			outcome: recall.SearchUnavailable,
			reason:  "unreachable",
			why:     "nothing was ever asked of the source, so it is unreachable rather than failed",
		},
		{
			name: "store cannot be opened",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{
					Stderr:   []byte("store.rb:2646: No such file or directory - tasks.jsonl (Errno::ENOENT)"),
					ExitCode: 1,
				}, nil
			},
			outcome: recall.SearchUnavailable,
			reason:  "unreachable",
			why:     "a non-zero exit from a read-only command means the store could not be read",
		},
		{
			name: "timeout",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{}, context.DeadlineExceeded
			},
			outcome: recall.SearchTimeout,
			reason:  "deadline_exceeded",
			why:     "a source that ran out of time is reported as timeout, never as an answer",
		},
		{
			name: "malformed JSON",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{Stdout: []byte(`[{"id":"aaaa0005",`)}, nil
			},
			outcome: recall.SearchFailed,
			reason:  "adapter_error",
			why:     "exit 0 with unreadable output is a broken contract, not an empty store",
		},
		{
			name: "empty output",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{Stdout: nil}, nil
			},
			outcome: recall.SearchFailed,
			reason:  "adapter_error",
			why:     "no bytes at all is not the same as the JSON array `[]`",
		},
		{
			name: "HTML error page instead of JSON",
			reply: func(args []string) (tasks.Result, error) {
				return tasks.Result{Stdout: []byte("<html>proxy error</html>")}, nil
			},
			outcome: recall.SearchFailed,
			reason:  "adapter_error",
			why:     "output that parses as nothing must not be coerced into an empty list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t, &fakeCLI{reply: tc.reply}, nil)

			resp, err := search(t, a, "vendor renewal")
			if err == nil {
				t.Fatal("search returned no error; a failed source must report one")
			}
			if resp.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q (%s)", resp.Outcome, tc.outcome, tc.why)
			}
			if resp.Outcome == recall.SearchSuccess {
				t.Error("a broken source reported success")
			}
			if len(resp.Candidates) != 0 {
				t.Errorf("got %d candidates from a failed search", len(resp.Candidates))
			}
			if got := resp.Diagnostics["reason"]; got != tc.reason {
				t.Errorf("reason = %v, want %q", got, tc.reason)
			}
		})
	}
}

// TestTimeoutIsEnforcedByTheAdapter checks the timeout is real rather than
// declarative: a CLI that never returns must still end the search, because the
// core's deadline enforcement kills a subprocess it owns and this adapter owns
// its own.
func TestTimeoutIsEnforcedByTheAdapter(t *testing.T) {
	a := newAdapter(t, wedgedCLI{}, map[string]any{"timeout_ms": 20})

	start := time.Now()
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query:    "anything",
		Deadline: time.Now().Add(time.Minute),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a wedged CLI produced a successful search")
	}
	if resp.Outcome != recall.SearchTimeout {
		t.Errorf("outcome = %q, want %q", resp.Outcome, recall.SearchTimeout)
	}
	if elapsed > time.Second {
		t.Errorf("search took %v; the configured 20ms timeout did not bound it", elapsed)
	}
}

// TestSearchAfterCloseFails guards the state machine. A closed adapter that
// answered would be worse than one that errored, because it would answer from
// a source it no longer holds.
func TestSearchAfterCloseFails(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resp, err := search(t, a, "vendor")
	if !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("err = %v, want adapter.ErrClosed", err)
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Error("a closed adapter reported success")
	}
}

// TestSearchBeforeInitializeFails covers the other end of the same machine.
func TestSearchBeforeInitializeFails(t *testing.T) {
	a := tasks.New(tasks.Options{Runner: recordedStore(t)})

	resp, err := search(t, a, "vendor")
	if err == nil {
		t.Fatal("an uninitialized adapter answered a search")
	}
	if resp.Outcome != recall.SearchUnavailable {
		t.Errorf("outcome = %q, want %q", resp.Outcome, recall.SearchUnavailable)
	}
}

// TestAsOfIsRefused is the honesty rule. The manifest declares as_of_support
// none, so the core should exclude this source; if a request arrives anyway,
// answering it from current state is the failure docs/spec.md names
// explicitly, and refusing costs one comparison.
func TestAsOfIsRefused(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	past := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query:    "vendor renewal",
		AsOf:     &past,
		Limit:    10,
		Deadline: time.Now().Add(time.Minute),
	})
	if !errors.Is(err, protocol.ErrAsOfUnsupported) {
		t.Fatalf("err = %v, want as_of_unsupported", err)
	}
	if len(resp.Candidates) != 0 {
		t.Error("a historical question was answered from current state")
	}
	if got := resp.Diagnostics["reason"]; got != "as_of_unsupported" {
		t.Errorf("reason = %v, want as_of_unsupported", got)
	}
}

// TestOnlyReadOnlyCommandsAreInvoked is defense in depth around a source that
// is specified as read-only. Every invocation the adapter makes across a full
// search, expand, and health cycle must name a command that cannot write.
func TestOnlyReadOnlyCommandsAreInvoked(t *testing.T) {
	// `id` prints an id but mints one when the record has none, so it writes
	// and is not on the allowlist despite reading like a query.
	mutating := map[string]bool{
		"done": true, "cancel": true, "state": true, "due": true, "schedule": true,
		"undate": true, "priority": true, "retitle": true, "tag": true, "note": true,
		"move": true, "recur": true, "defer": true, "someday": true, "activate": true,
		"delete": true, "capture": true, "archive": true, "undo": true, "redo": true,
		"project": true, "migrate": true, "id": true,
	}

	cli := recordedStore(t)
	a := newAdapter(t, cli, nil)

	if _, err := search(t, a, "vendor renewal "+knownID); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "tasks", Local: knownID},
		Detail:  recall.DetailFull,
	}); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}

	calls := cli.invocations()
	if len(calls) == 0 {
		t.Fatal("no invocations recorded")
	}
	for _, call := range calls {
		if mutating[call[0]] {
			t.Errorf("invoked a mutating command: tasks %s", strings.Join(call, " "))
		}
		for _, arg := range call[1:] {
			// Query text reaches the CLI only inside a `/text` filter, so no
			// bare positional argument may appear except a validated id.
			if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") && len(arg) != 8 {
				t.Errorf("bare argument %q in: tasks %s", arg, strings.Join(call, " "))
			}
		}
	}
}
