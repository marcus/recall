package docs_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/recall"
)

// TestDotDirectoriesAreNotCorpus. Tool state lives in dot-directories, and some
// of it — agent worktrees, vendored checkouts — is a copy of the corpus itself.
// Indexing those copies is not merely noisy: each copy carries a different
// path, so one document becomes several source_record_ids, lineage cannot group
// them, and the document ends up corroborating itself in the fused result.
func TestDotDirectoriesAreNotCorpus(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	const marker = "porcupine ranking decision"
	writeFile(t, filepath.Join(root, "notes.md"), "# Notes\n\n"+marker+"\n")

	// A worktree-shaped copy of the same document: exactly what an agent
	// harness leaves behind inside a working repository.
	writeFile(t, filepath.Join(root, ".claude", "worktrees", "agent-1", "notes.md"),
		"# Notes\n\n"+marker+"\n")
	writeFile(t, filepath.Join(root, ".venv", "lib", "readme.md"), "# Venv\n\n"+marker+"\n")

	a, _ := newAdapter(t, root, map[string]any{"extensions": []any{".md"}})
	res := search(t, a, marker)

	if res.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q, want success", res.Outcome)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("no candidates: the document outside the dot-directories must still be indexed")
	}
	for _, c := range res.Candidates {
		local := c.Locator.Local
		if strings.HasPrefix(local, ".") || strings.Contains(local, "/.") {
			t.Errorf("indexed a dot-directory: %q", local)
		}
	}
	if got := len(res.Candidates); got != 1 {
		t.Errorf("candidates = %d, want 1: the copies must not become separate records", got)
	}
}
