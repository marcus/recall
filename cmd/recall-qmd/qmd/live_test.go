package qmd_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// These tests drive the real qmd binary. Everything above them is a fixture, and
// a fixture can only prove that this adapter reads what a recording says qmd
// wrote — not that qmd still writes it. That is the gap this file closes: the
// snippet header a locator's line range is parsed out of, the `--explain` trace
// the per-result attribution is built from, and the three text reports the
// identity and coverage checks rest on are all qmd's output format, and none of
// them is a documented interface.
//
// Two rules keep them from being a nuisance:
//
//   - The index is project-local, built with `qmd init` inside a copy of
//     testdata/corpus in a temporary directory. Nothing touches the developer's
//     own qmd index or collections, which is also why [qmd.ExecRunner] anchors
//     its working directory on the corpus rather than inheriting Recall's.
//   - The model-backed modes run only when the models are already on the
//     machine. A first `qmd embed` downloads about 2GB, and a test suite must
//     never be the thing that starts that.

func qmdBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("qmd")
	if err != nil {
		t.Skip("qmd is not on PATH; the fixture suites cover the same code paths")
	}
	return path
}

// modelsPresent reports whether the embedding model is already cached.
//
// It reads qmd's cache directory, which is peeking at another program's
// internals, and the alternative is worse: an environment variable nobody sets
// leaves the model-backed paths unexercised everywhere, and running them
// unconditionally makes `go test` download 2GB on a fresh machine.
// RECALL_QMD_LIVE_MODELS=1 forces them on.
func modelsPresent(t *testing.T) bool {
	t.Helper()
	if os.Getenv("RECALL_QMD_LIVE_MODELS") == "1" {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dir := filepath.Join(home, ".cache", "qmd", "models")
	if override := os.Getenv("QMD_CACHE_DIR"); override != "" {
		dir = filepath.Join(override, "models")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Name()), "embed") {
			return true
		}
	}
	return false
}

// liveCorpus copies the committed corpus into a temporary directory and indexes
// it into a project-local qmd index.
func liveCorpus(t *testing.T, embed bool) string {
	t.Helper()
	binary := qmdBinary(t)
	root := t.TempDir()
	src := filepath.Join("..", "testdata", "corpus")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	var copyTree func(rel string)
	copyTree = func(rel string) {
		items, err := os.ReadDir(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			child := filepath.Join(rel, item.Name())
			if item.IsDir() {
				if err := os.MkdirAll(filepath.Join(root, child), 0o755); err != nil {
					t.Fatal(err)
				}
				copyTree(child)
				continue
			}
			body, err := os.ReadFile(filepath.Join(src, child))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, child), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(entries) == 0 {
		t.Fatal("testdata/corpus is empty")
	}
	copyTree(".")

	run := func(timeout time.Duration, args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("qmd %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(30*time.Second, "init")
	run(30*time.Second, "collection", "add", ".", "--name", "fixture", "--mask", "**/*.md")
	run(60*time.Second, "update")
	if embed {
		run(5*time.Minute, "embed", "-c", "fixture")
	}
	return root
}

func liveAdapter(t *testing.T, root, mode string) *qmd.Adapter {
	t.Helper()
	a := qmd.New(qmd.Options{})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: root,
		Settings: map[string]any{"collection": "fixture", "mode": mode, "timeout_ms": 120000},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// The load-bearing parse. A locator without a line range makes expansion
// impossible, and the range comes out of a snippet header qmd does not document.
func TestLiveBM25LocatorRoundTrip(t *testing.T) {
	root := liveCorpus(t, false)
	a := liveAdapter(t, root, "bm25")

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental hygienist accepting new patients", Limit: 10,
		Deadline: time.Now().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q: %v", resp.Outcome, resp.Diagnostics)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("the live index returned nothing for a query whose terms are in the corpus")
	}
	c := resp.Candidates[0]
	if c.SourceRecordID != "notes/tooth-care.md" {
		t.Errorf("source_record_id = %q, want the collection-relative path", c.SourceRecordID)
	}
	if !strings.Contains(c.Locator.Local, "#L") {
		t.Fatalf("locator %q carries no line range", c.Locator.Local)
	}
	if c.Relevance == nil || *c.Relevance <= 0 {
		t.Errorf("relevance = %v for a lexical hit on its own terms", c.Relevance)
	}
	if c.ExcerptKind != recall.ExcerptMatched {
		t.Errorf("excerpt_kind = %q", c.ExcerptKind)
	}

	// The locator is a promise, and this is the whole of it: expanding it returns
	// the lines it names.
	got, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: c.Locator, Detail: recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("expanding %q: %v", c.Locator.Local, err)
	}
	if strings.TrimSpace(got.Content) == "" {
		t.Fatal("the locator expanded to nothing")
	}
	if !strings.Contains(got.Provenance, c.SourceRecordID) {
		t.Errorf("provenance = %q", got.Provenance)
	}
}

// bm25 abstains honestly. It is the anchor every semantic claim is measured
// against, and it is the one mode whose empty result is trustworthy: the
// reranked modes score an off-corpus query's nearest document at the same value
// a genuinely relevant one earns.
func TestLiveBM25AbstainsOffCorpus(t *testing.T) {
	root := liveCorpus(t, false)
	a := liveAdapter(t, root, "bm25")

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "quantum chromodynamics lattice gauge", Limit: 10,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 0 {
		t.Fatalf("outcome = %q with %d candidates, want an honest empty success",
			resp.Outcome, len(resp.Candidates))
	}
}

// The three text reports. A qmd release that reworded any of them must surface
// as a source that cannot confirm itself, and this is the test that notices.
func TestLiveHealthReadsTheRealReports(t *testing.T) {
	root := liveCorpus(t, false)
	a := liveAdapter(t, root, "bm25")

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !health.Usable() {
		t.Fatalf("health = %q: %v", health.Status, health.Diagnostics)
	}
	if health.RecordCount == 0 {
		t.Error("qmd status reported no files for the collection")
	}
	if !strings.Contains(health.IndexConfig, "qmd=") ||
		strings.Contains(health.IndexConfig, "qmd=unknown") {
		t.Errorf("index_config = %q, want the real qmd version", health.IndexConfig)
	}
	if health.IndexGeneration == "" || health.IndexWatermark == "" {
		t.Errorf("generation = %q watermark = %q", health.IndexGeneration, health.IndexWatermark)
	}
	id, _ := health.Diagnostics[protocol.DiagStoreIdentity].(string)
	if id == "" {
		t.Error("a live source published no store identity")
	}
	if strings.Contains(id, root) {
		t.Errorf("store identity carries a path: %q", id)
	}
	for key, value := range health.Diagnostics {
		if text, ok := value.(string); ok && strings.Contains(text, root) {
			t.Errorf("diagnostic %q carries an absolute path: %q", key, text)
		}
	}
}

// A collection re-pointed at another tree, produced for real: `qmd collection
// add` is how it happens, and this is the check that keeps a search from
// answering out of the wrong corpus.
func TestLiveCollectionMismatchIsRefused(t *testing.T) {
	binary := qmdBinary(t)
	root := liveCorpus(t, false)
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "README.md"),
		[]byte("# Another corpus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-point the collection at another directory, then ask the adapter
	// configured at the original one. qmd refuses to add a name it already holds,
	// so the re-point is a remove and an add — which is exactly how an operator
	// would do it, and therefore exactly how this happens by accident.
	for _, args := range [][]string{
		{"collection", "remove", "fixture"},
		{"collection", "add", elsewhere, "--name", "fixture", "--mask", "**/*.md"},
	} {
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("re-pointing the collection with %v: %v\n%s", args, err, out)
		}
	}
	a := liveAdapter(t, root, "bm25")

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental hygienist", Limit: 10, Deadline: time.Now().Add(time.Minute),
	})
	if err == nil {
		t.Fatalf("a search answered from an unverified corpus: %d candidates", len(resp.Candidates))
	}
	if outcome, _ := adapter.Classify(err); outcome != recall.SearchUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
}

// The paraphrase case, live: a question whose words appear nowhere in the
// document that answers it. bm25 returns nothing and the vector mode finds it,
// which is the entire reason this source exists — and the recomputed relevance
// is zero for it, which is the limitation stated in doc.go.
func TestLiveVectorFindsAParaphrase(t *testing.T) {
	if !modelsPresent(t) {
		t.Skip("qmd's models are not cached; a first embed downloads about 2GB")
	}
	root := liveCorpus(t, true)

	lexical := liveAdapter(t, root, "bm25")
	resp, err := lexical.Search(context.Background(), recall.SearchRequest{
		Query: "who can clean my teeth", Limit: 10,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 0 {
		t.Skipf("the corpus has drifted: bm25 answers the paraphrase with %d results",
			len(resp.Candidates))
	}

	semantic := liveAdapter(t, root, "vector")
	got, err := semantic.Search(context.Background(), recall.SearchRequest{
		Query: "who can clean my teeth", Limit: 10,
		Deadline: time.Now().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) == 0 {
		t.Fatal("the vector mode found nothing for a paraphrase of a document in the corpus")
	}
	if got.Candidates[0].SourceRecordID != "notes/tooth-care.md" {
		t.Errorf("first result = %q, want notes/tooth-care.md", got.Candidates[0].SourceRecordID)
	}
	if !got.Candidates[0].HasSignal(recall.MatchSemantic) {
		t.Error("a vector hit must carry the semantic signal")
	}
	if got.Candidates[0].Relevance == nil {
		t.Fatal("relevance was omitted, which reads as 1.0")
	}
	if *got.Candidates[0].Relevance != 0 {
		// Not a failure: if the shared lexical definition ever scores this above
		// zero the corpus has changed, and the limitation in doc.go should be
		// re-examined rather than silently assumed.
		t.Logf("paraphrase relevance is %v; doc.go assumes 0 for a pure paraphrase",
			*got.Candidates[0].Relevance)
	}
}

// `--explain` is the whole attribution story, and it is a JSON shape qmd does
// not document.
func TestLiveHybridExplainTrace(t *testing.T) {
	if !modelsPresent(t) {
		t.Skip("qmd's models are not cached; a first embed downloads about 2GB")
	}
	root := liveCorpus(t, true)
	a := liveAdapter(t, root, "hybrid")

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "who can clean my teeth", Limit: 10,
		Deadline: time.Now().Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("the hybrid mode returned nothing")
	}
	signals, ok := resp.Candidates[0].Metadata["signals"].(map[string]any)
	if !ok {
		t.Fatalf("no per-result signals: %v", resp.Candidates[0].Metadata)
	}
	for _, key := range []string{"rrf_rank", "rrf_score", "components"} {
		if _, ok := signals[key]; !ok {
			t.Errorf("signals missing %q: %v", key, signals)
		}
	}
	if _, ok := signals["rerank_score"]; ok {
		t.Error("hybrid mode reported a rerank score it did not compute")
	}
	if _, ok := resp.Diagnostics["expanded_queries"]; !ok {
		t.Error("no expanded queries: expansion is the layer this reports on")
	}
}

// Refresh against the real tool: reindex, embed, and warm, then report health.
func TestLiveRefresh(t *testing.T) {
	if !modelsPresent(t) {
		t.Skip("qmd's models are not cached; a first embed downloads about 2GB")
	}
	root := liveCorpus(t, false)
	a := liveAdapter(t, root, "hybrid")

	// Before a refresh the index holds documents and no vectors, so an
	// embedding-backed search cannot see the corpus at all.
	if _, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental hygienist", Limit: 10, Deadline: time.Now().Add(time.Minute),
	}); err == nil {
		t.Error("a hybrid search answered from an index holding no vectors")
	}

	health, err := a.Refresh(context.Background(), protocol.RefreshParams{
		Deadline: time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthHealthy || health.Coverage != recall.IndexComplete {
		t.Fatalf("health after refresh = %q/%q: %v",
			health.Status, health.Coverage, health.Diagnostics)
	}
	if _, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental hygienist", Limit: 10, Deadline: time.Now().Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("a search after a refresh failed: %v", err)
	}
}
