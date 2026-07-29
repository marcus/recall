package docs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/pkg/recall"
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

// TestSettingsChangeRebuildsTheIndex. Settings decide what a generation
// contains, so a generation built under different settings does not describe
// the corpus the current configuration asks for — not one file has to change
// for that to be true.
//
// Reopening it and reporting degraded would not be enough: nothing triggers a
// rebuild on its own, so the source would answer from the old boundary
// indefinitely while every search reported success. The handshake rebuilds
// instead, which is the cost the first handshake already pays.
func TestSettingsChangeRebuildsTheIndex(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	workdir := t.TempDir()

	first := docs.New()
	if _, err := first.Initialize(context.Background(), config(root, workdir, nil)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	before := health(t, first)
	if before.Status != recall.HealthHealthy {
		t.Fatalf("status = %q before any change: %v", before.Status, before.Diagnostics)
	}
	_ = first.Close()
	published := currentGeneration(t, workdir)

	second := docs.New()
	t.Cleanup(func() { _ = second.Close() })
	settings := map[string]any{"extensions": []any{".md", ".txt"}}
	if _, err := second.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("second handshake: %v", err)
	}

	h := health(t, second)
	if h.IndexGeneration == published {
		t.Fatalf("generation %q survived a settings change that redefines the corpus", published)
	}
	if h.Status != recall.HealthHealthy {
		t.Errorf("status = %q after a rebuild under the new settings: %v", h.Status, h.Diagnostics)
	}
	if h.IndexWatermark != h.SourceWatermark {
		t.Errorf("index watermark %q != source watermark %q right after the rebuild",
			h.IndexWatermark, h.SourceWatermark)
	}
	if h.IndexWatermark == before.IndexWatermark {
		t.Error("the watermark did not move with the settings that decide what is indexed")
	}
	if resp := search(t, second, "plaintextonly"); len(resp.Candidates) == 0 {
		t.Error("the rebuilt generation does not contain the file the new settings admit")
	}
}

// TestUnchangedSettingsReopenTheGeneration is the other half of the rule above:
// a handshake under the settings a generation was built under must not rebuild,
// or every restart would pay for a full corpus walk it does not need.
func TestUnchangedSettingsReopenTheGeneration(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	workdir := t.TempDir()
	settings := map[string]any{"extensions": []any{".md"}, "exclude_dirs": []any{"node_modules"}}

	first := docs.New()
	if _, err := first.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = first.Close()
	published := currentGeneration(t, workdir)

	second := docs.New()
	t.Cleanup(func() { _ = second.Close() })
	if _, err := second.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("second handshake: %v", err)
	}
	if got := currentGeneration(t, workdir); got != published {
		t.Errorf("handshake rebuilt %q into %q under identical settings", published, got)
	}
}

// TestExcludeDirsChangeRebuildsTheIndex. The exclusions decide what the corpus
// IS, so they are in the settings digest exactly as the extensions are: an
// index built while .github/ was excluded must not keep answering after someone
// admitted it, reporting complete coverage over the old boundary.
func TestExcludeDirsChangeRebuildsTheIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workdir := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "# Notes\n\nranking decisions\n")
	writeFile(t, filepath.Join(root, ".github", "CONTRIBUTING.md"),
		"# Contributing\n\nopen a pull request against main\n")

	first := docs.New()
	if _, err := first.Initialize(context.Background(), config(root, workdir, nil)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	before := health(t, first)
	if before.RecordCount != 1 {
		t.Fatalf("record_count = %d, want 1 under the default exclusions", before.RecordCount)
	}
	_ = first.Close()

	second := docs.New()
	t.Cleanup(func() { _ = second.Close() })
	settings := map[string]any{"exclude_dirs": []any{"node_modules"}}
	if _, err := second.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("second handshake: %v", err)
	}

	h := health(t, second)
	if h.IndexGeneration == before.IndexGeneration {
		t.Fatalf("generation %q survived a change to the excluded directories", before.IndexGeneration)
	}
	if h.RecordCount != 2 || h.IndexedCount != 2 {
		t.Errorf("record_count = %d, indexed_count = %d, want 2: the admitted directory is corpus now",
			h.RecordCount, h.IndexedCount)
	}
	if h.IndexConfig == before.IndexConfig {
		t.Errorf("index_config is %q for both boundaries; a corpus change nothing records is a change nobody can attribute",
			h.IndexConfig)
	}
	if resp := search(t, second, "pull request against main"); len(resp.Candidates) == 0 {
		t.Error("the rebuilt generation does not contain the admitted directory's documents")
	}
}

// TestMalformedExclusionsFailTheHandshake. A pattern that cannot match is an
// exclusion that looks configured and excludes nothing, which is the silent
// boundary this setting exists to make visible.
func TestMalformedExclusionsFailTheHandshake(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings map[string]any
		wants    string
	}{
		{"a path", map[string]any{"exclude_dirs": []any{"docs/vendor"}}, "docs/vendor"},
		{"a bad glob", map[string]any{"exclude_dirs": []any{"[unclosed"}}, "[unclosed"},
		{"an empty name", map[string]any{"exclude_dirs": []any{"  "}}, "empty"},
		{"a non-boolean", map[string]any{"exclude_nested_repos": "yes"}, "exclude_nested_repos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := docs.New()
			t.Cleanup(func() { _ = a.Close() })
			_, err := a.Initialize(context.Background(), config(cleanCorpus(t), t.TempDir(), tc.settings))
			if err == nil {
				t.Fatalf("handshake accepted %v", tc.settings)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not name what was wrong (%q)", err, tc.wants)
			}
		})
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
