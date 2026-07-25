package docs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitRevisionIsRecordedAsFreshnessEvidence.
//
// The revision is evidence, not the watermark: a working tree can be edited
// without moving HEAD, so an index keyed on the revision alone would claim to
// be current after every uncommitted edit. Both facts are reported, and
// equality is decided by the file digest.
//
// The repository is written by hand rather than by running git: the three files
// below are a stable on-disk format, and an adapter that shelled out would
// inherit another program's PATH, exit codes, and prompts.
func TestGitRevisionIsRecordedAsFreshnessEvidence(t *testing.T) {
	t.Parallel()
	const sha = "0badc0de1234567890abcdef0123456789abcdef"

	cases := []struct {
		name  string
		write func(t *testing.T, gitDir string)
	}{
		{
			name: "loose ref",
			write: func(t *testing.T, gitDir string) {
				writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
				writeFile(t, filepath.Join(gitDir, "refs", "heads", "main"), sha+"\n")
			},
		},
		{
			name: "packed ref",
			write: func(t *testing.T, gitDir string) {
				writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
				writeFile(t, filepath.Join(gitDir, "packed-refs"),
					"# pack-refs with: peeled fully-peeled sorted\n"+sha+" refs/heads/main\n")
			},
		},
		{
			name: "detached head",
			write: func(t *testing.T, gitDir string) {
				writeFile(t, filepath.Join(gitDir, "HEAD"), sha+"\n")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := cleanCorpus(t)
			tc.write(t, filepath.Join(root, ".git"))

			a, _ := newAdapter(t, root, nil)
			h := health(t, a)

			if got, _ := h.Diagnostics["git_revision"].(string); got != sha {
				t.Errorf("git_revision = %q, want %q", got, sha)
			}
			if !strings.HasPrefix(h.SourceWatermark, "git:"+sha[:12]+"+fs:") {
				t.Errorf("watermark %q does not carry the revision and the file digest", h.SourceWatermark)
			}

			// The repository's own object store is not corpus content.
			resp := search(t, a, "pack refs peeled ref heads main")
			for _, c := range resp.Candidates {
				if strings.HasPrefix(c.SourceRecordID, ".git/") {
					t.Errorf("indexed %s from inside the repository metadata", c.SourceRecordID)
				}
			}
		})
	}
}

// TestGitRevisionFoundFromASubdirectory: a corpus is usually a directory
// inside a repository, not the repository root.
func TestGitRevisionFoundFromASubdirectory(t *testing.T) {
	t.Parallel()
	const sha = "abcdef01234567890abcdef01234567890abcdef"
	root := cleanCorpus(t)
	writeFile(t, filepath.Join(root, ".git", "HEAD"), sha+"\n")

	a, _ := newAdapter(t, root, map[string]any{"root": filepath.Join(root, "projects", "recall")})
	if got, _ := health(t, a).Diagnostics["git_revision"].(string); got != sha {
		t.Errorf("git_revision = %q, want %q from the repository above the corpus", got, sha)
	}
}

// TestNoGitRepositoryIsNotAnError. Most corpora are ordinary directories; a
// missing revision is normal and must not degrade anything.
func TestNoGitRepositoryIsNotAnError(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			t.Skipf("temporary directories live inside a repository at %s", dir)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	a, _ := newAdapter(t, root, nil)

	h := health(t, a)
	if _, ok := h.Diagnostics["git_revision"]; ok {
		t.Errorf("a plain directory reported a revision: %v", h.Diagnostics)
	}
	if !strings.HasPrefix(h.SourceWatermark, "fs:") {
		t.Errorf("watermark = %q, want a bare file digest", h.SourceWatermark)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
