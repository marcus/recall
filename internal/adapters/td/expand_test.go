package td_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// The detail levels widen rather than reshape: each one's output starts with
// the previous one's, so a caller comparing a summary against a full expansion
// sees added lines and not rewritten ones.
func TestDetailLevelsWiden(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)
	local := "tdfix/" + idAdapter

	summary, err := expand(t, a, local, recall.DetailSummary, 0)
	if err != nil {
		t.Fatalf("expand summary: %v", err)
	}
	full, err := expand(t, a, local, recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("expand full: %v", err)
	}
	if !strings.HasPrefix(full.Content, summary.Content) {
		t.Errorf("full expansion does not start with the summary:\n%s\n---\n%s", summary.Content, full.Content)
	}

	// A summary is the structured row: what it is, where it sits, what state
	// it is in. It stops before the prose.
	for _, want := range []string{idAdapter, "P1", "Adapter interface", "epic: " + idEpic, "labels: wave1"} {
		if !strings.Contains(summary.Content, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary.Content)
		}
	}
	if strings.Contains(summary.Content, "SIGTERM") {
		t.Errorf("summary carries the description body:\n%s", summary.Content)
	}

	// Full is where the record's own history arrives: the description, the
	// acceptance criteria, and the log — which is exactly the material td's
	// own search cannot see, so expansion is the only way to reach it.
	for _, want := range []string{
		"cancel notification, then SIGTERM",
		"Acceptance:",
		"A hanging fixture adapter is killed at its deadline",
		"Supervision, deadlines, pooling landed",
	} {
		if !strings.Contains(full.Content, want) {
			t.Errorf("full expansion is missing %q:\n%s", want, full.Content)
		}
	}
}

// Context is the one level that means something beyond the record itself, and
// the only one that costs extra invocations.
func TestContextAddsTheSurroundingWork(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	resp, err := expand(t, a, "tdfix/"+idIndexing, recall.DetailContext, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(resp.Content, "Depends on:\n- "+idAdapter) {
		t.Errorf("context does not name what this issue waits on:\n%s", resp.Content)
	}
	if cli.countCalls("depends-on") != 1 || cli.countCalls("blocked-by") != 1 || cli.countCalls("files") != 1 {
		t.Error("context expansion did not gather the surrounding work")
	}

	full, err := expand(t, a, "tdfix/"+idIndexing, recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if strings.Contains(full.Content, "Depends on") {
		t.Error("a full expansion paid for the dependency graph it did not ask for")
	}
}

func TestBudgetTruncationIsReportedWithItsBoundary(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := expand(t, a, "tdfix/"+idAdapter, recall.DetailFull, 80)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("an expansion cut to 80 bytes did not report truncation")
	}
	if resp.TruncationBoundary != "budget_bytes" {
		t.Errorf("truncation_boundary = %q, want budget_bytes: a caller has to tell a budget cut from a source limit",
			resp.TruncationBoundary)
	}
	if int64(len(resp.Content)) > 80 {
		t.Errorf("content is %d bytes, over the 80-byte budget", len(resp.Content))
	}
}

// Provenance names the workspace, not only the issue. Two workspaces can hold
// ids of the same shape, so evidence saying only "td-277316" would be a
// reference nobody could resolve.
func TestProvenanceNamesTheWorkspace(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := expand(t, a, "tdfix/"+idAdapter, recall.DetailSummary, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, want := range []string{"tdfix", idAdapter} {
		if !strings.Contains(resp.Provenance, want) {
			t.Errorf("provenance %q is missing %q", resp.Provenance, want)
		}
	}
	if strings.Contains(resp.Provenance, workspaceRoot) {
		t.Errorf("provenance discloses absolute workspace root: %q", resp.Provenance)
	}
	// The issue's own last-write time is the closest thing td has to a
	// revision of one record, and it does not move when an unrelated issue
	// changes.
	if !strings.HasPrefix(resp.SourceRevision, "2026-07-25T05:54:32") {
		t.Errorf("source_revision = %q, want this issue's updated_at", resp.SourceRevision)
	}
}

// The workspace boundary, enforced. One adapter serves many workspaces, and a
// locator from another one must not be answered from this one — the ids are
// six random hex characters, so the wrong answer would look entirely
// plausible.
func TestALocatorFromAnotherWorkspaceIsRefused(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	_, err := expand(t, a, "braid/"+idAdapter, recall.DetailFull, 0)
	if err == nil {
		t.Fatal("a locator naming another workspace was expanded")
	}
	if !strings.Contains(err.Error(), "braid") || !strings.Contains(err.Error(), "tdfix") {
		t.Errorf("error %q does not name both workspaces", err)
	}
	if cli.countCalls("show") != 0 {
		t.Error("td was asked about another workspace's locator")
	}
}

func TestLocatorForms(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	// The qualified form is what this adapter emits.
	if _, err := expand(t, a, "tdfix/"+idAdapter, recall.DetailSummary, 0); err != nil {
		t.Errorf("qualified locator: %v", err)
	}
	// The bare form is accepted too: naming this instance is naming its
	// workspace, and refusing it would be pedantry rather than safety.
	if _, err := expand(t, a, idAdapter, recall.DetailSummary, 0); err != nil {
		t.Errorf("bare locator: %v", err)
	}
	// Anything that is not a td id is not a locator, however it is spelled.
	for _, local := range []string{"tdfix/nope", "tdfix/TD-277316", "tdfix/277316", ""} {
		if _, err := expand(t, a, local, recall.DetailSummary, 0); err == nil {
			t.Errorf("%q was accepted as a locator", local)
		}
	}
}

// An issue the workspace no longer holds is an expired locator, not a nearby
// record and not an empty answer.
func TestExpandingAMissingIssueExpires(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	_, err := expand(t, a, "tdfix/"+idNotPresent, recall.DetailFull, 0)
	if err == nil {
		t.Fatal("expanding a missing issue succeeded")
	}
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Errorf("err = %v, want locator_expired", err)
	}
}
