package td_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Losing every probe is a failed search even when the listing answered. The
// listing is not a search: returning it unfiltered would answer a question
// nobody asked, and returning nothing would claim the workspace holds no match.
func TestEveryProbeFailingIsAFailureAndNotAnEmptyAnswer(t *testing.T) {
	listAll := fixture(t, "list_all.json")
	cli := &fakeCLI{reply: func(args []string) (td.Result, error) {
		if args[0] == "list" {
			return ok(listAll), nil
		}
		return td.Result{Stdout: fixture(t, "no_database.json"), ExitCode: 1}, nil
	}}
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("a search whose every probe failed returned no error")
	}
	if resp.Outcome != recall.SearchUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("a failed search returned %d candidates", len(resp.Candidates))
	}
}

// Losing some probes is degradation, not failure: the answer is real and
// thinner than it should be, and saying partial is what stops it being read as
// complete.
func TestOneFailingProbeDegradesRatherThanFails(t *testing.T) {
	recorded := recordedWorkspace(t)
	cli := &fakeCLI{reply: func(args []string) (td.Result, error) {
		if args[0] == "search" && args[len(args)-1] == "corroboration" {
			return td.Result{Stdout: fixture(t, "no_database.json"), ExitCode: 1}, nil
		}
		return recorded.reply(args)
	}}
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter corroboration lineage")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %q, want partial", resp.Outcome)
	}
	if resp.Diagnostics["failed_probes"] != 1 {
		t.Errorf("diagnostics[failed_probes] = %v, want 1", resp.Diagnostics["failed_probes"])
	}
	if len(resp.Candidates) == 0 {
		t.Error("the probes that did answer produced nothing")
	}
}

// The listing is what confirms records present and what fingerprints the
// workspace state. Losing it leaves a real answer with no freshness evidence,
// which is partial.
func TestLosingTheListingLeavesAPartialAnswer(t *testing.T) {
	recorded := recordedWorkspace(t)
	cli := &fakeCLI{reply: func(args []string) (td.Result, error) {
		if args[0] == "list" {
			return td.Result{Stdout: fixture(t, "no_database.json"), ExitCode: 1}, nil
		}
		return recorded.reply(args)
	}}
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %q, want partial", resp.Outcome)
	}
	if resp.Diagnostics["corpus"] != "unavailable" {
		t.Errorf("diagnostics[corpus] = %v, want unavailable", resp.Diagnostics["corpus"])
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("the probes answered but nothing was returned")
	}
	for _, c := range resp.Candidates {
		if c.ConfirmedAt != nil {
			t.Errorf("%s is confirmed present with no listing to confirm it", c.SourceRecordID)
		}
	}
}

// A wedged td is a timeout, not an unavailable source and not an empty
// result. The distinction is what the core reports and what a person acts on.
func TestAWedgedProcessIsATimeout(t *testing.T) {
	a := newAdapter(t, wedgedCLI{}, map[string]any{"timeout_ms": 25})

	start := time.Now()
	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("a wedged td produced no error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if resp.Outcome != recall.SearchTimeout {
		t.Errorf("outcome = %q, want timeout", resp.Outcome)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the declared timeout did not bound the call: %v", elapsed)
	}
}

// The caller's deadline still wins when it is sooner than the adapter's
// configured timeout. The per-invocation timeout composes with the request
// budget rather than replacing it, so a source that decided on a generous
// timeout can never overrun the budget it was given.
func TestTheSoonerOfDeadlineAndTimeoutWins(t *testing.T) {
	a := newAdapter(t, wedgedCLI{}, map[string]any{"timeout_ms": 60000})

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := a.Search(ctx, recall.SearchRequest{
		Query: "adapter", Limit: 20, Deadline: time.Now().Add(25 * time.Millisecond),
	})
	if err == nil {
		t.Fatal("a wedged td produced no error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the request deadline did not bound the call: %v", elapsed)
	}
}

// Output that is not JSON means the contract broke. It is a failure, and
// specifically not an unreachable source: td ran and answered.
func TestUnreadableOutputFailsRatherThanReturningNothing(t *testing.T) {
	cli := &fakeCLI{reply: func([]string) (td.Result, error) {
		return td.Result{Stdout: []byte("<html>upgrade required</html>")}, nil
	}}
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("unreadable output was accepted")
	}
	if resp.Outcome != recall.SearchFailed {
		t.Errorf("outcome = %q, want failed", resp.Outcome)
	}
}

// A binary that cannot be run at all is unreachable rather than failed:
// nothing was ever asked of the workspace.
func TestAMissingBinaryIsUnreachable(t *testing.T) {
	a := newAdapter(t, nil, map[string]any{"binary": "td-does-not-exist"})

	// The adapter was built with a nil Runner and a binary name, so this
	// exercises the real ExecRunner against a name PATH cannot resolve.
	resp, err := search(t, a, "adapter")
	if err == nil {
		t.Fatal("a missing binary produced no error")
	}
	if !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Errorf("err = %v, want source_unavailable", err)
	}
	if resp.Outcome != recall.SearchUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
}
