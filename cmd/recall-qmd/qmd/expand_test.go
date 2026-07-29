package qmd_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

func expand(t *testing.T, root string, local string, detail recall.DetailLevel, budget int64) (recall.ExpandResponse, error) {
	t.Helper()
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, "[]"))
	return a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "qmd", Local: local},
		Detail:   detail,
		Budget:   budget,
		Deadline: time.Now().Add(time.Minute),
	})
}

func TestExpandServesEachDetailLevel(t *testing.T) {
	root := corpus(t)
	local := "notes/tooth-care.md#L3-L5"
	for _, detail := range []recall.DetailLevel{
		recall.DetailSummary, recall.DetailExcerpt, recall.DetailFull, recall.DetailContext,
	} {
		t.Run(string(detail), func(t *testing.T) {
			resp, err := expand(t, root, local, detail, 0)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content == "" {
				t.Fatal("no evidence")
			}
			if !strings.Contains(resp.Provenance, "notes/tooth-care.md") {
				t.Errorf("provenance = %q", resp.Provenance)
			}
			if !strings.HasPrefix(resp.SourceRevision, "file:") {
				t.Errorf("source_revision = %q, want a content digest", resp.SourceRevision)
			}
		})
	}
}

// The revision is derived from content alone, so two machines reading one
// unchanged file report the same one — and an edited file reports a different
// one without anything else having to notice.
func TestExpandRevisionFollowsContent(t *testing.T) {
	root := corpus(t)
	first, err := expand(t, root, "notes/tooth-care.md#L1-L3", recall.DetailFull, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := expand(t, root, "notes/tooth-care.md#L1-L3", recall.DetailFull, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceRevision != again.SourceRevision {
		t.Fatal("an unchanged file reported two revisions")
	}
	path := filepath.Join(root, "notes", "tooth-care.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte("\nA new line.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	edited, err := expand(t, root, "notes/tooth-care.md#L1-L3", recall.DetailFull, 0)
	if err != nil {
		t.Fatal(err)
	}
	if edited.SourceRevision == first.SourceRevision {
		t.Fatal("an edited file reported the same revision")
	}
}

func TestExpandTruncatesAtTheStatedBoundary(t *testing.T) {
	root := corpus(t)
	resp, err := expand(t, root, "notes/tooth-care.md#L1-L9", recall.DetailFull, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Truncated || resp.TruncationBoundary != "budget_bytes" {
		t.Fatalf("truncated = %v boundary = %q", resp.Truncated, resp.TruncationBoundary)
	}
	if len(resp.Content) > 100 {
		t.Fatalf("content is %d bytes over a 100-byte budget", len(resp.Content))
	}
	if !strings.Contains(resp.Content, "truncated") {
		t.Error("a cut must be marked")
	}
}

// A budget too small to carry evidence and its marker is refused up front rather
// than answered with a marker and nothing else.
func TestExpandRefusesAnImpossibleBudget(t *testing.T) {
	root := corpus(t)
	_, err := expand(t, root, "notes/tooth-care.md#L1-L9", recall.DetailFull, 16)
	if err == nil {
		t.Fatal("a 16-byte budget was accepted")
	}
	if !isCode(err, protocol.CodeBudgetExceeded) {
		t.Fatalf("error = %v, want budget_exceeded", err)
	}
}

// A locator that parses and no longer resolves is locator_expired; one that does
// not parse is locator_unknown. Conflating them tells a caller to fix the wrong
// thing.
func TestExpandDistinguishesUnknownFromExpired(t *testing.T) {
	root := corpus(t)
	unknown := []string{
		"notes/tooth-care.md",                // no range
		"notes/tooth-care.md#3-9",            // range is not L<start>-L<end>
		"notes/tooth-care.md#L9-L3",          // end before start
		"notes/tooth-care.md#L0-L3",          // lines are one-based
		"../outside.md#L1-L2",                // escapes the corpus
		"/etc/passwd#L1-L2",                  // absolute
		"notes/../notes/tooth-care.md#L1-L2", // not in normal form
	}
	for _, local := range unknown {
		if _, err := expand(t, root, local, recall.DetailExcerpt, 0); !isCode(err, protocol.CodeLocatorUnknown) {
			t.Errorf("%q gave %v, want locator_unknown", local, err)
		}
	}
	expired := []string{
		"notes/gone.md#L1-L4",           // never in this corpus, or deleted
		"notes/tooth-care.md#L900-L910", // past the end of the file
	}
	for _, local := range expired {
		if _, err := expand(t, root, local, recall.DetailExcerpt, 0); !isCode(err, protocol.CodeLocatorExpired) {
			t.Errorf("%q gave %v, want locator_expired", local, err)
		}
	}
}

// A range whose end has fallen past the end of the file serves the lines that
// remain and says how far it got. It never substitutes a neighbouring range.
func TestExpandClampsHonestly(t *testing.T) {
	root := corpus(t)
	resp, err := expand(t, root, "notes/tooth-care.md#L8-L40", recall.DetailFull, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Provenance, "L8-L9") {
		t.Fatalf("provenance = %q, want the range actually served", resp.Provenance)
	}
	if !resp.Truncated || resp.TruncationBoundary != "file_end" {
		t.Fatalf("truncated = %v boundary = %q, want the clamp reported",
			resp.Truncated, resp.TruncationBoundary)
	}
}

// Expansion reads the file, so a locator a caller already holds still resolves
// when qmd cannot run at all — except for the one check that keeps it from
// reading the wrong tree, which is why a mismatch is refused instead.
func TestExpandDoesNotAskQmdForEvidence(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	a := newAdapter(t, root, baseSettings(), runner)
	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "qmd", Local: "notes/tooth-care.md#L1-L3"},
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	for _, args := range runner.invocations() {
		switch args[0] {
		case "query", "search", "vsearch", "status":
			t.Errorf("expansion ran %q; evidence comes from the file", args[0])
		}
	}
}

// Control characters, bidi overrides, and CR are stripped from evidence a
// terminal and a model both read.
func TestExpandSanitizesEvidence(t *testing.T) {
	root := corpus(t)
	// Written as escapes on purpose. A source file that spelled these out would
	// carry an invisible bidi override into the next tool that read it.
	hostile := "# Title\r\n\u202eoverride\u202c\x07bell\n\u2028separator\n"
	if err := os.WriteFile(filepath.Join(root, "notes", "hostile.md"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := expand(t, root, "notes/hostile.md#L1-L4", recall.DetailFull, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\r", "\u202e", "\u202c", "\x07", "\u2028"} {
		if strings.Contains(resp.Content, bad) {
			t.Errorf("evidence carries %q", bad)
		}
	}
	if !strings.Contains(resp.Content, "override") {
		t.Error("stripping removed the text as well as the controls")
	}
}

func isCode(err error, code protocol.Code) bool {
	var got *protocol.Error
	if err == nil || !errors.As(err, &got) {
		return false
	}
	return got.Code == code
}
