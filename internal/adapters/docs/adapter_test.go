package docs_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// The adapter's on-disk layout, restated here because the tests assert on it.
// Atomic publication is a property of these names: a pointer file that is
// renamed into place, generation directories that are never edited, and staging
// directories a failed build leaves behind.
const (
	indexSubdir = "index"
	currentFile = "current"
	genPrefix   = "gen-"
	buildPrefix = "build-"
)

// corpus copies the committed fixture corpus into a temp directory.
//
// Tests mutate their corpus — deleting files, adding files, breaking
// permissions — so none of them may share one. The copy is what makes "a file
// deleted upstream" a real deletion rather than a simulated one.
func corpus(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "corpus")
	src := filepath.Join("testdata", "corpus")

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	return dst
}

// cleanCorpus is the fixture without the file that cannot be indexed, for tests
// about a complete boundary rather than a partial one.
func cleanCorpus(t *testing.T) string {
	t.Helper()
	root := corpus(t)
	if err := os.RemoveAll(filepath.Join(root, "broken")); err != nil {
		t.Fatalf("remove broken fixture: %v", err)
	}
	return root
}

// writtenCorpus is a corpus written for one test, for the tests whose subject
// is term statistics: which words are rare and which are everywhere is the
// thing under test, and the shared fixture cannot state that per test.
func writtenCorpus(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(name), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// newAdapter completes a handshake against root and returns the adapter with
// its workdir. Initialize publishes the first generation, so every test starts
// from a source that has actually indexed something.
func newAdapter(t *testing.T, root string, settings map[string]any) (*docs.Adapter, string) {
	t.Helper()
	workdir := t.TempDir()
	a := docs.New()
	t.Cleanup(func() { _ = a.Close() })

	if _, err := a.Initialize(context.Background(), config(root, workdir, settings)); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return a, workdir
}

func config(root, workdir string, settings map[string]any) adapter.Config {
	return adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            workdir,
		SourceID:           "docs",
		Location:           root,
		Settings:           settings,
	}
}

func search(t *testing.T, a *docs.Adapter, query string, opts ...func(*recall.SearchRequest)) recall.SearchResponse {
	t.Helper()
	req := recall.SearchRequest{Query: query, Limit: 50, Deadline: time.Now().Add(10 * time.Second)}
	for _, opt := range opts {
		opt(&req)
	}
	resp, err := a.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return resp
}

func health(t *testing.T, a *docs.Adapter) recall.Health {
	t.Helper()
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	return h
}

// currentGeneration reads the published pointer directly, which is the only
// thing a concurrent reader of this workdir would ever look at.
func currentGeneration(t *testing.T, workdir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(workdir, indexSubdir, currentFile))
	if err != nil {
		t.Fatalf("read published pointer: %v", err)
	}
	return strings.TrimSpace(string(body))
}

func indexEntries(t *testing.T, workdir string, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(workdir, indexSubdir))
	if err != nil {
		t.Fatalf("read index dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// metaString reads a typed metadata field a candidate is required to carry.
func metaString(t *testing.T, c recall.Candidate, key string) string {
	t.Helper()
	v, ok := c.Metadata[key].(string)
	if !ok {
		t.Fatalf("candidate %s: metadata %q is %T, want string", c.CandidateID, key, c.Metadata[key])
	}
	return v
}

func metaInt(t *testing.T, c recall.Candidate, key string) int {
	t.Helper()
	v, ok := c.Metadata[key].(int)
	if !ok {
		t.Fatalf("candidate %s: metadata %q is %T, want int", c.CandidateID, key, c.Metadata[key])
	}
	return v
}

// lineOf finds a line by prefix, so assertions about line ranges survive an
// edit to the fixture text.
func lineOf(t *testing.T, root, rel, prefix string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	for i, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, prefix) {
			return i + 1
		}
	}
	t.Fatalf("%s has no line starting %q", rel, prefix)
	return 0
}

func firstFrom(t *testing.T, resp recall.SearchResponse, path string) recall.Candidate {
	t.Helper()
	for _, c := range resp.Candidates {
		if c.SourceRecordID == path {
			return c
		}
	}
	t.Fatalf("no candidate from %s in %d results", path, len(resp.Candidates))
	return recall.Candidate{}
}

// TestManifestDeclaresOnlyWhatItCanServe pins the handshake claims. Each of
// these is a promise the core routes on: a false one sends this adapter
// requests it would answer badly, which is worse than not answering.
func TestManifestDeclaresOnlyWhatItCanServe(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	a := docs.New()
	t.Cleanup(func() { _ = a.Close() })

	manifest, err := a.Initialize(context.Background(), config(cleanCorpus(t), workdir, nil))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}

	if manifest.AsOfSupport != recall.AsOfFilter {
		t.Errorf("as_of_support = %q, want %q: a filesystem cannot reconstruct a document's earlier text",
			manifest.AsOfSupport, recall.AsOfFilter)
	}
	if slices.Contains(manifest.QueryModes, recall.QuerySemantic) {
		t.Error("query_modes claims semantic; this adapter is lexical only")
	}
	if !slices.Contains(manifest.QueryModes, recall.QueryLexical) {
		t.Error("query_modes must declare lexical")
	}
	if !manifest.Supports(recall.FreshnessIndexed) {
		t.Error("freshness_modes must declare indexed")
	}
	if manifest.Supports(recall.FreshnessLive) {
		t.Error("freshness_modes claims live; search reads the published generation")
	}
	if !manifest.Can(recall.CapSearch) || !manifest.Can(recall.CapExpand) {
		t.Error("capabilities must declare search and expand")
	}
	if manifest.SettingsSchema == nil {
		t.Error("settings_schema is how recall doctor checks configuration without a query")
	}
	if manifest.FreshnessPolicy == "" {
		t.Error("freshness_policy states which partial boundary may report healthy")
	}
}

// TestUnknownSettingFailsHandshake keeps a typo from becoming a silently
// ignored configuration. The adapter is the only thing that can check its own
// settings on a first handshake, because the core has not seen the schema yet.
func TestUnknownSettingFailsHandshake(t *testing.T) {
	t.Parallel()
	a := docs.New()
	t.Cleanup(func() { _ = a.Close() })

	_, err := a.Initialize(context.Background(),
		config(cleanCorpus(t), t.TempDir(), map[string]any{"extension": []any{".md"}}))
	if err == nil {
		t.Fatal("handshake accepted an unknown setting")
	}
	if !strings.Contains(err.Error(), "extension") {
		t.Errorf("error %q does not name the setting that was wrong", err)
	}
}

// TestWritesNothingOutsideWorkdir holds the handshake's promise from the other
// side: the corpus is read-only to this adapter, and every byte it persists
// lands under the directory it was given.
func TestWritesNothingOutsideWorkdir(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	before := treeOf(t, root)

	a, workdir := newAdapter(t, root, nil)
	search(t, a, "corroboration counts units")
	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if after := treeOf(t, root); !slices.Equal(before, after) {
		t.Errorf("corpus changed:\nbefore %v\nafter  %v", before, after)
	}
	for _, name := range treeOf(t, workdir) {
		if !strings.HasPrefix(name, indexSubdir+string(filepath.Separator)) {
			t.Errorf("workdir entry %q is outside the index directory", name)
		}
	}
}

func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}
