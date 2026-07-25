package docs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

func expandReq(loc recall.Locator, detail recall.DetailLevel, budget int64) recall.ExpandRequest {
	return recall.ExpandRequest{
		Locator:  loc,
		Detail:   detail,
		Budget:   budget,
		Deadline: time.Now().Add(10 * time.Second),
	}
}

// locatorFor returns a locator for a section of the fixture, taken from search
// the way a caller would get one.
func locatorFor(t *testing.T, a interface {
	Search(context.Context, recall.SearchRequest) (recall.SearchResponse, error)
}, query, doc string,
) recall.Locator {
	t.Helper()
	resp, err := a.Search(context.Background(), recall.SearchRequest{Query: query, Limit: 50})
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	for _, c := range resp.Candidates {
		if c.SourceRecordID == doc {
			return c.Locator
		}
	}
	t.Fatalf("no candidate from %s for %q", doc, query)
	return recall.Locator{}
}

// TestExpandAtEveryDetailLevel. Each level is a promise about size, and a
// caller budgets against it; a level that ignored its bound would blow a token
// budget that was allocated on the strength of the name.
func TestExpandAtEveryDetailLevel(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doc = "projects/recall/architecture.md"
	loc := locatorFor(t, a, "generation published single rename interrupted build", doc)

	cases := []struct {
		name      string
		detail    recall.DetailLevel
		truncated bool
		boundary  string
		check     func(t *testing.T, resp recall.ExpandResponse, full recall.ExpandResponse)
	}{
		{
			name:   "full is the whole section",
			detail: recall.DetailFull,
			check: func(t *testing.T, resp, _ recall.ExpandResponse) {
				if !strings.Contains(resp.Content, "## Indexing") {
					t.Error("full expansion does not include the section heading")
				}
				if !strings.Contains(resp.Content, "single rename") {
					t.Error("full expansion is missing the section body")
				}
			},
		},
		{
			name:   "summary names the section and its first line",
			detail: recall.DetailSummary,
			check: func(t *testing.T, resp, full recall.ExpandResponse) {
				if len(resp.Content) >= len(full.Content) {
					t.Errorf("summary is %d bytes, not smaller than the full %d",
						len(resp.Content), len(full.Content))
				}
				if !strings.HasPrefix(resp.Content, "Recall Architecture > Indexing") {
					t.Errorf("summary does not open with the heading path: %q", resp.Content)
				}
			},
		},
		{
			name:   "excerpt is the head of the section",
			detail: recall.DetailExcerpt,
			check: func(t *testing.T, resp, full recall.ExpandResponse) {
				if len(resp.Content) > len(full.Content) {
					t.Error("excerpt is larger than the full section")
				}
			},
		},
		{
			name:   "context reaches past the section",
			detail: recall.DetailContext,
			check: func(t *testing.T, resp, full recall.ExpandResponse) {
				if len(resp.Content) <= len(full.Content) {
					t.Errorf("context (%d bytes) is no larger than the section itself (%d)",
						len(resp.Content), len(full.Content))
				}
				if !strings.Contains(resp.Content, "Corroboration") {
					t.Error("context does not reach the neighboring section")
				}
			},
		},
	}

	full, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0))
	if err != nil {
		t.Fatalf("expand full: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := a.Expand(context.Background(), expandReq(loc, tc.detail, 0))
			if err != nil {
				t.Fatalf("expand %s: %v", tc.detail, err)
			}
			if resp.Provenance == "" {
				t.Error("no provenance: evidence without a path and range cannot be checked")
			}
			if !strings.HasPrefix(resp.Provenance, doc+":L") {
				t.Errorf("provenance %q does not name the file and line range", resp.Provenance)
			}
			if resp.SourceRevision == "" {
				t.Error("no source revision: evidence must say which revision it was read from")
			}
			tc.check(t, resp, full)
		})
	}
}

// TestExpandMarksEveryTruncation. A caller has to be able to tell a bounded
// answer from a complete one, and a budget cut from a detail-level cut: only
// the first is worth retrying with a larger budget.
func TestExpandMarksEveryTruncation(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doc = "projects/recall/architecture.md"
	loc := locatorFor(t, a, "generation published single rename interrupted build", doc)

	t.Run("budget", func(t *testing.T) {
		const budget = 120
		resp, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, budget))
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if int64(len(resp.Content)) > budget {
			t.Errorf("content is %d bytes over a budget of %d", len(resp.Content), budget)
		}
		if !resp.Truncated || resp.TruncationBoundary != "budget_bytes" {
			t.Errorf("truncated = %v, boundary = %q, want a budget_bytes cut",
				resp.Truncated, resp.TruncationBoundary)
		}
		if !strings.Contains(resp.Content, "truncated") {
			t.Errorf("no truncation marker in the evidence: %q", resp.Content)
		}
	})

	t.Run("budget too small to answer at all", func(t *testing.T) {
		_, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 8))
		if !errors.Is(err, protocol.ErrBudgetExceeded) {
			t.Errorf("expand within 8 bytes: %v, want budget_exceeded", err)
		}
	})

	t.Run("detail level", func(t *testing.T) {
		// A long section: the excerpt level has to cut it at its own line
		// bound, and say that is what happened.
		long := filepath.Join(root, "projects", "recall", "long.md")
		var body strings.Builder
		body.WriteString("# Long\n\n## Section\n\n")
		for range 40 {
			body.WriteString("distinctivetoken filler line about publication.\n")
		}
		if err := os.WriteFile(long, []byte(body.String()), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
			t.Fatalf("rebuild: %v", err)
		}

		loc := locatorFor(t, a, "distinctivetoken", "projects/recall/long.md")
		resp, err := a.Expand(context.Background(), expandReq(loc, recall.DetailExcerpt, 0))
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if !resp.Truncated || resp.TruncationBoundary != "excerpt_lines" {
			t.Errorf("truncated = %v, boundary = %q, want an excerpt_lines cut",
				resp.Truncated, resp.TruncationBoundary)
		}
		if !strings.Contains(resp.Content, "excerpt_lines") {
			t.Error("the evidence carries no marker naming the boundary that applied")
		}

		whole, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0))
		if err != nil {
			t.Fatalf("expand full: %v", err)
		}
		if whole.Truncated {
			t.Error("the full section reports truncation")
		}
	})
}

// TestExpandRefusesLocatorsItDidNotIssue. A locator is text that travelled
// through the core and back; anything it names outside the corpus this instance
// was configured for must be refused before a file is opened.
func TestExpandRefusesLocatorsItDidNotIssue(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	cases := []struct {
		name  string
		local string
		want  error
	}{
		{"no line range", "projects/recall/status.md", protocol.ErrLocatorUnknown},
		{"malformed range", "projects/recall/status.md#lines-3-9", protocol.ErrLocatorUnknown},
		{"reversed range", "projects/recall/status.md#L9-L3", protocol.ErrLocatorUnknown},
		{"parent escape", "../../../etc/passwd#L1-L2", protocol.ErrLocatorUnknown},
		{"absolute path", "/etc/passwd#L1-L2", protocol.ErrLocatorUnknown},
		{"unnormalized path", "projects/./recall/status.md#L1-L2", protocol.ErrLocatorUnknown},
		{"unknown document", "projects/recall/never-existed.md#L1-L2", protocol.ErrLocatorExpired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Expand(context.Background(),
				expandReq(recall.Locator{SourceID: "docs", Local: tc.local}, recall.DetailFull, 0))
			if !errors.Is(err, tc.want) {
				t.Errorf("expand %q: %v, want %v", tc.local, err, tc.want)
			}
		})
	}
}

// TestExpandReportsARevisionThatMovedPastTheIndex. Expansion reads the file,
// not the index, so an edit since the last build changes what comes back. That
// is allowed — it is the current document — but it is never silent.
func TestExpandReportsARevisionThatMovedPastTheIndex(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doc = "projects/recall/status.md"
	loc := locatorFor(t, a, "lexical baseline landed ranking stable", doc)

	before, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0))
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	path := filepath.Join(root, filepath.FromSlash(doc))
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, append(body, []byte("\nAn edit after the build.\n")...), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	after, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0))
	if err != nil {
		t.Fatalf("expand after the edit: %v", err)
	}
	if after.SourceRevision == before.SourceRevision {
		t.Errorf("revision is still %q after the file changed", after.SourceRevision)
	}
	if !strings.HasPrefix(after.SourceRevision, "file:") {
		t.Errorf("revision %q does not say the evidence came from a file past the generation",
			after.SourceRevision)
	}
}

// TestExpandFailsWhenTheRangeIsGone. A file truncated past the locator's start
// no longer contains what was retrieved, and a nearby range is not the same
// evidence.
func TestExpandFailsWhenTheRangeIsGone(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doc = "projects/recall/decisions.md"
	loc := locatorFor(t, a, "priors bounded reviewed range explanation", doc)

	path := filepath.Join(root, filepath.FromSlash(doc))
	if err := os.WriteFile(path, []byte("# Recall Decisions\n"), 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	_, err := a.Expand(context.Background(), expandReq(loc, recall.DetailFull, 0))
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Errorf("expand of a range past the end of the file: %v, want locator_expired", err)
	}
}
