package tasks_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

func expand(t *testing.T, a *tasks.Adapter, local string, detail recall.DetailLevel, budget int64) (recall.ExpandResponse, error) {
	t.Helper()
	return a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "tasks", Local: local},
		Detail:  detail,
		Budget:  budget,
	})
}

// TestExpandDetailLevelsWiden checks each level does something distinct. A
// level that returned the same bytes as its neighbour would be a contract
// field with no code path behind it, which docs/spec.md calls a defect.
func TestExpandDetailLevelsWiden(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	// aaaa000c has a note and a link, so every level has something to add.
	summary, err := expand(t, a, "aaaa000c", recall.DetailSummary, 0)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	full, err := expand(t, a, "aaaa000c", recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("full: %v", err)
	}

	if strings.Contains(summary.Content, "Draft lives at") {
		t.Error("summary includes note prose; notes are where a task stops being a structured row")
	}
	if !strings.Contains(full.Content, "Draft lives at") {
		t.Error("full omits the note")
	}
	if !strings.HasPrefix(full.Content, summary.Content) {
		t.Errorf("full does not start with summary; the levels reshape rather than widen\nsummary:\n%s\nfull:\n%s",
			summary.Content, full.Content)
	}
	if !strings.Contains(full.Content, "https://example.test/docs/landing") {
		t.Error("full omits the extracted link")
	}
	if !strings.Contains(summary.Content, "project: Launch the personal site") {
		t.Error("summary omits the project, which is routing information a summary needs")
	}
	if full.SourceRevision == "" {
		t.Error("no source_revision; evidence must say which revision it was read from")
	}
	if !strings.Contains(full.Provenance, "aaaa000c") {
		t.Errorf("provenance = %q, want the record reference", full.Provenance)
	}
}

// TestExpandBudgetTruncates. A budget is a hard output limit, and a caller has
// to be able to tell a short record from a cut one.
func TestExpandBudgetTruncates(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := expand(t, a, "aaaa000c", recall.DetailFull, 40)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !resp.Truncated {
		t.Error("truncated = false for content cut to fit a budget")
	}
	if resp.TruncationBoundary != "budget_bytes" {
		t.Errorf("truncation_boundary = %q, want budget_bytes so a budget cut is distinguishable from a source limit",
			resp.TruncationBoundary)
	}
	if len(resp.Content) > 41 { // the ellipsis is one rune past the cut
		t.Errorf("content is %d bytes, over the 40-byte budget", len(resp.Content))
	}
}

// TestExpandRejectsNonLocators covers the locator contract: the local part is
// the stable id and nothing else. The CLI happily resolves a title substring
// or an L<line> reference, which is exactly why they must not be accepted here —
// they move, and they resolve ambiguously.
func TestExpandRejectsNonLocators(t *testing.T) {
	tests := []struct {
		name  string
		local string
		why   string
	}{
		{"title substring", "landing", "the CLI would resolve it, to a record that could change"},
		{"line reference", "L12", "line numbers move whenever anything above them moves"},
		{"uppercase id", "AAAA000C", "an id has one spelling"},
		{"prefix", "aaaa000", "a prefix is not an identity"},
		{"empty", "", "there is nothing to resolve"},
	}

	a := newAdapter(t, recordedStore(t), nil)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := expand(t, a, tc.local, recall.DetailFull, 0)
			if !errors.Is(err, protocol.ErrLocatorUnknown) {
				t.Errorf("err = %v, want locator_unknown (%s)", err, tc.why)
			}
		})
	}
}

// TestExpandVanishedRecordExpires. A record the store no longer holds must
// fail rather than resolve to whatever the CLI thought was closest.
func TestExpandVanishedRecordExpires(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	_, err := expand(t, a, "deadbeef", recall.DetailFull, 0)
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("err = %v, want locator_expired", err)
	}
}

// TestExpandNeverSubstitutesANearbyRecord is the same rule against the CLI's
// fuzzy resolution: `show` answers 0 for a title substring, so a record whose
// id differs from the one asked for is a different record, not this one.
func TestExpandNeverSubstitutesANearbyRecord(t *testing.T) {
	cli := recordedStore(t)
	inner := cli.reply
	cli.reply = func(args []string) (tasks.Result, error) {
		if args[0] == "show" {
			// Whatever was asked for, answer with aaaa0005.
			return tasks.Result{Stdout: fixture(t, "show_task.json")}, nil
		}
		return inner(args)
	}
	a := newAdapter(t, cli, nil)

	_, err := expand(t, a, "aaaa000c", recall.DetailFull, 0)
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("err = %v, want locator_expired rather than a nearby record", err)
	}
}

// TestExpandFallsBackToTheListingForArchivedRecords: `tasks show` reads the
// live file only, so a record swept to the archive is not gone — reporting it
// expired would be a false claim about the source.
func TestExpandFallsBackToTheListingForArchivedRecords(t *testing.T) {
	cli := recordedStore(t)
	inner := cli.reply
	cli.reply = func(args []string) (tasks.Result, error) {
		if args[0] == "show" {
			return tasks.Result{Stderr: fixture(t, "show_not_found.txt"), ExitCode: 2}, nil
		}
		return inner(args)
	}
	a := newAdapter(t, cli, nil)

	// aaaa0005 is absent from `show` here but present in the full listing.
	resp, err := expand(t, a, "aaaa0005", recall.DetailFull, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(resp.Content, "Reply to the vendor") {
		t.Errorf("content = %q, want the record recovered from the listing", resp.Content)
	}
}

// TestExpandContextAddsSiblings. `context` is the only detail level that means
// something beyond the record, so it is the only one that costs extra
// invocations — and it has to actually spend them.
func TestExpandContextAddsSiblings(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := expand(t, a, "aaaa000c", recall.DetailContext, 0)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !strings.Contains(resp.Content, "Alongside in this project") {
		t.Errorf("context detail returned no surrounding work:\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, "Pick a static-site generator") {
		t.Error("the sibling task in the same project is missing")
	}
}

// TestExpandAfterCloseFails. A closed adapter must not read the source.
func TestExpandAfterCloseFails(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := expand(t, a, "aaaa0005", recall.DetailFull, 0); err == nil {
		t.Fatal("a closed adapter expanded a locator")
	}
}
