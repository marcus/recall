package docs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/recall"
)

// Every setting below decides what the index contains. A setting with no test
// is configuration nobody can rely on, which the spec counts as a defect.

// TestExtensionsDecideWhatIsADocument. The fixture keeps a .txt file that the
// default settings ignore; declaring the suffix must bring it in.
func TestExtensionsDecideWhatIsADocument(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)

	byDefault, _ := newAdapter(t, root, nil)
	if resp := search(t, byDefault, "plaintextonly"); len(resp.Candidates) != 0 {
		t.Errorf("a .txt file was indexed without being declared: %s", resp.Candidates[0].Locator.Local)
	}

	declared, _ := newAdapter(t, root, map[string]any{"extensions": []any{".md", ".txt"}})
	resp := search(t, declared, "plaintextonly")
	if len(resp.Candidates) == 0 {
		t.Fatal("declaring .txt did not index the .txt file")
	}
	if got := resp.Candidates[0].SourceRecordID; got != "notes.txt" {
		t.Errorf("matched %q, want notes.txt", got)
	}
}

// TestMaxFileBytesFailsOneRecord. A file too large to be a document is one
// failed record, not a failed build: the setting bounds work per file, and one
// pathological file must not cost the corpus its index.
func TestMaxFileBytesFailsOneRecord(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	body := strings.Repeat("# Big\n\nfiller about publication.\n", 200)
	if err := os.WriteFile(filepath.Join(root, "big.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	a, _ := newAdapter(t, root, map[string]any{"max_file_bytes": 1024})
	h := health(t, a)
	if h.FailedCount != 1 {
		t.Fatalf("failed_count = %d, want 1: %v", h.FailedCount, h.Diagnostics)
	}
	if h.Coverage != recall.IndexPartial {
		t.Errorf("coverage = %q, want partial", h.Coverage)
	}
	if h.IndexedCount == 0 {
		t.Error("one oversized file cost the whole corpus its index")
	}
}

// TestRootOverridesLocation lets an instance point at a directory narrower than
// its configured location.
func TestRootOverridesLocation(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, map[string]any{"root": filepath.Join(root, "projects", "clara")})

	resp := search(t, a, "signals memory ranking corroboration")
	if len(resp.Candidates) == 0 {
		t.Fatal("settings.root indexed nothing")
	}
	for _, c := range resp.Candidates {
		if !strings.HasPrefix(c.SourceRecordID, "notes.md") {
			t.Errorf("%s is outside the configured root", c.SourceRecordID)
		}
	}
}

// TestSettingsChangeMakesTheIndexStale. Settings decide what a generation
// contains, so a generation built under different settings is not current, even
// though not one file changed.
func TestSettingsChangeMakesTheIndexStale(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	workdir := t.TempDir()

	first := docs.New()
	if _, err := first.Initialize(context.Background(), config(root, workdir, nil)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if h := health(t, first); h.Status != recall.HealthHealthy {
		t.Fatalf("status = %q before any change: %v", h.Status, h.Diagnostics)
	}
	_ = first.Close()

	second := docs.New()
	t.Cleanup(func() { _ = second.Close() })
	settings := map[string]any{"extensions": []any{".md", ".txt"}}
	if _, err := second.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("second handshake: %v", err)
	}
	// The handshake reopened the published generation rather than rebuilding,
	// so the source keeps answering — but it must not claim to be current under
	// configuration it was not built under.
	h := health(t, second)
	if stale, _ := h.Diagnostics["stale"].(bool); !stale {
		t.Errorf("health does not mark the generation stale after a settings change: %v", h.Diagnostics)
	}
	if h.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded", h.Status)
	}
	if h.IndexWatermark == h.SourceWatermark {
		t.Error("the watermark did not move with the settings that decide what is indexed")
	}
}

// TestElapsedDeadlineIsATimeout. Every request carries a deadline. A source
// that started work it could not finish would spend the caller's remaining
// budget and return nothing, and reporting that as an empty success is the
// failure mode invariant 2 exists for.
func TestElapsedDeadlineIsATimeout(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	past := time.Now().Add(-time.Second)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "corroboration", Limit: 5, Deadline: past,
	})
	if err == nil {
		t.Fatal("search past its deadline reported success")
	}
	if resp.Outcome != recall.SearchTimeout {
		t.Errorf("outcome = %q, want timeout", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Error("a timed-out search returned candidates")
	}
}

// TestClosedAdapterIsNeverAnEmptySuccess. Same invariant, other cause.
func TestClosedAdapterIsNeverAnEmptySuccess(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resp, err := a.Search(context.Background(), recall.SearchRequest{Query: "corroboration", Limit: 5})
	if err == nil || resp.Outcome == recall.SearchSuccess {
		t.Errorf("search after close: outcome %q, err %v", resp.Outcome, err)
	}
	if _, err := a.Health(context.Background()); err == nil {
		t.Error("health after close reported a state")
	}
}

// TestAsOfExcludesDocumentsChangedAfterTheBoundary. The manifest declares
// filter, not snapshot: a document modified after the boundary is excluded
// entirely, because the filesystem cannot produce its earlier text and
// answering from the current text would answer a historical question from
// current state.
func TestAsOfExcludesDocumentsChangedAfterTheBoundary(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)

	const recent = "projects/recall/status.md"
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(recent)), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	a, _ := newAdapter(t, root, nil)

	all := search(t, a, "lexical baseline ranking corroboration indexing")
	var sawRecent bool
	for _, c := range all.Candidates {
		sawRecent = sawRecent || c.SourceRecordID == recent
	}
	if !sawRecent {
		t.Fatalf("%s is missing from an unbounded search", recent)
	}

	boundary := time.Now()
	bounded := search(t, a, "lexical baseline ranking corroboration indexing",
		func(req *recall.SearchRequest) { req.AsOf = &boundary })
	for _, c := range bounded.Candidates {
		if c.SourceRecordID == recent {
			t.Errorf("%s answered a request bounded before its own modification time", recent)
		}
	}
	if len(bounded.Candidates) == 0 {
		t.Error("as_of excluded the whole corpus")
	}
	if got, _ := bounded.Diagnostics["as_of"].(string); got == "" {
		t.Error("diagnostics do not say how as_of was honored")
	}
}
