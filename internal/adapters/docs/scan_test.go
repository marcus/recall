package docs_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

// TestExcludedDirectoriesAreNotCorpus. Tool state lives in dot-directories, and
// some of it — agent worktrees, vendored checkouts — is a copy of the corpus
// itself. Indexing those copies is not merely noisy: each copy carries a
// different path, so one document becomes several source_record_ids, lineage
// cannot group them, and the document ends up corroborating itself in the fused
// result.
//
// The assertion is on record_count, not on how many candidates came back. A
// candidate count is a fact about chunking and ranking; the corpus boundary is
// what this rule is about, and it is what record_count reports.
func TestExcludedDirectoriesAreNotCorpus(t *testing.T) {
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

	if got := health(t, a).RecordCount; got != 1 {
		t.Errorf("record_count = %d, want 1: the corpus is the one document outside the tool state", got)
	}
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
}

// TestSkippedDirectoriesAreReported. An exclusion nobody can see is the failure
// this configuration exists to prevent: a source that answers "nothing found,
// coverage complete" over content inside the root the operator named. Health
// reports the count beside coverage and a bounded sample naming the rule, and
// it does NOT report them as failures — a corpus with a .git/ would otherwise
// be partial forever, which destroys the signal that says records are missing.
func TestSkippedDirectoriesAreReported(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "# Notes\n\nranking decisions\n")
	writeFile(t, filepath.Join(root, ".github", "CONTRIBUTING.md"), "# Contributing\n\nhow to send a patch\n")
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "readme.md"), "# Pkg\n\nvendored\n")

	a, _ := newAdapter(t, root, nil)
	h := health(t, a)

	if got, _ := h.Diagnostics["skipped_dirs"].(int); got != 2 {
		t.Errorf("skipped_dirs = %v, want 2: %v", h.Diagnostics["skipped_dirs"], h.Diagnostics)
	}
	sample, _ := h.Diagnostics["skipped_dirs_sample"].([]string)
	if len(sample) != 2 {
		t.Fatalf("skipped_dirs_sample = %v, want both directories named", sample)
	}
	joined := strings.Join(sample, " ")
	for _, want := range []string{".github", "node_modules", "exclude_dirs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("sample %v does not say %q was excluded or why", sample, want)
		}
	}
	if h.FailedCount != 0 {
		t.Errorf("failed_count = %d: a skipped directory is not a failed record", h.FailedCount)
	}
	if h.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q: excluding tool state does not make the declared boundary partial", h.Coverage)
	}
	if h.IndexConfig == "" {
		t.Error("index_config is unset; a corpus boundary change must be recorded in the generation")
	}
}

// TestDeclaredExclusionsDecideTheBoundary. exclude_dirs is configuration, not a
// rule: a corpus whose prose lives in .github/ says so and gets it, and the
// generation is rebuilt because the setting changed what the corpus IS.
func TestDeclaredExclusionsDecideTheBoundary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".github", "CONTRIBUTING.md"),
		"# Contributing\n\nopen a pull request against main\n")

	byDefault, _ := newAdapter(t, root, nil)
	if resp := search(t, byDefault, "pull request against main"); len(resp.Candidates) != 0 {
		t.Errorf("the default excluded .github but indexed it anyway: %s", resp.Candidates[0].Locator.Local)
	}

	declared, _ := newAdapter(t, root, map[string]any{"exclude_dirs": []any{"node_modules"}})
	resp := search(t, declared, "pull request against main")
	if len(resp.Candidates) == 0 {
		t.Fatal("declaring exclude_dirs without the dot-class did not index .github")
	}
	if got := resp.Candidates[0].SourceRecordID; got != ".github/CONTRIBUTING.md" {
		t.Errorf("matched %q, want .github/CONTRIBUTING.md", got)
	}
}

// TestADotDirectoryCanBeACorpus is the escape hatch, held this time. Pointing a
// source straight at `.claude` used to walk `worktrees/agent-1/` and index the
// checkout below it — the original duplication bug, reintroduced by the
// documented workaround. Nested-repository detection is what closes it: the
// copy carries a .git entry of its own, so it is never entered, whatever the
// name patterns say.
func TestADotDirectoryCanBeACorpus(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	root := filepath.Join(outer, ".claude")

	const marker = "porcupine ranking decision"
	writeFile(t, filepath.Join(root, "notes.md"), "# Notes\n\n"+marker+"\n")
	// A worktree keeps a .git FILE naming the real repository; a clone keeps a
	// .git directory. Both are a second checkout of the same documents.
	writeFile(t, filepath.Join(root, "worktrees", "agent-1", "notes.md"), "# Notes\n\n"+marker+"\n")
	writeFile(t, filepath.Join(root, "worktrees", "agent-1", ".git"), "gitdir: /elsewhere/.git/worktrees/agent-1\n")
	writeFile(t, filepath.Join(root, "worktrees", "agent-2", "notes.md"), "# Notes\n\n"+marker+"\n")
	writeFile(t, filepath.Join(root, "worktrees", "agent-2", ".git", "HEAD"), "ref: refs/heads/main\n")

	a, _ := newAdapter(t, root, map[string]any{"extensions": []any{".md"}})

	h := health(t, a)
	if h.RecordCount != 1 {
		t.Errorf("record_count = %d, want 1: the checkouts under the corpus are copies, not records", h.RecordCount)
	}
	if got, _ := h.Diagnostics["skipped_dirs"].(int); got != 2 {
		t.Errorf("skipped_dirs = %v, want the two checkouts: %v", h.Diagnostics["skipped_dirs"], h.Diagnostics)
	}
	if sample, _ := h.Diagnostics["skipped_dirs_sample"].([]string); !strings.Contains(strings.Join(sample, " "), "nested repository") {
		t.Errorf("sample %v does not say the checkouts were skipped as repositories", sample)
	}

	res := search(t, a, marker)
	if len(res.Candidates) == 0 {
		t.Fatal("a corpus rooted at a dot-directory indexed nothing")
	}
	for _, c := range res.Candidates {
		if c.SourceRecordID != "notes.md" {
			t.Errorf("indexed %q from inside a nested checkout", c.SourceRecordID)
		}
	}
}

// TestNestedRepositoriesCanBeAdmitted. The nested-repository rule is declared
// too. A corpus of independently versioned document repositories is a real
// shape, and a rule it could not turn off would cost it everything below the
// first checkout with nothing in the configuration to say so.
func TestNestedRepositoriesCanBeAdmitted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "# Notes\n\nranking decisions\n")
	writeFile(t, filepath.Join(root, "handbook", "onboarding.md"), "# Onboarding\n\nfirst week checklist\n")
	writeFile(t, filepath.Join(root, "handbook", ".git", "HEAD"), "ref: refs/heads/main\n")

	a, _ := newAdapter(t, root, map[string]any{"exclude_nested_repos": false})
	h := health(t, a)
	if h.RecordCount != 2 {
		t.Fatalf("record_count = %d, want 2: the nested repository was declared to be corpus", h.RecordCount)
	}
	if got, _ := h.Diagnostics["skipped_dirs"].(int); got != 1 {
		t.Errorf("skipped_dirs = %v, want 1 for the .git directory itself: %v",
			h.Diagnostics["skipped_dirs"], h.Diagnostics)
	}
	if resp := search(t, a, "first week checklist"); len(resp.Candidates) == 0 {
		t.Error("the admitted repository's documents are not searchable")
	}
}
