package td

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveBaseDirChecksLocalAssociationBeforeGitRoot(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "api")
	local := filepath.Join(repo, "nested")
	target := filepath.Join(t.TempDir(), "api")
	for _, dir := range []string{local, target} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}

	got := resolveBaseDirUsing(local, func(dir string) (string, bool) {
		if filepath.Clean(dir) == filepath.Clean(local) {
			return target, true
		}
		return "", false
	})
	if got != target {
		t.Fatalf("resolved %s, want local association %s before git root %s", got, target, repo)
	}
}

func TestResolveRootIncludesDBOpenSecondResolutionPass(t *testing.T) {
	start := filepath.Join(t.TempDir(), "start")
	middle := filepath.Join(t.TempDir(), "middle")
	final := filepath.Join(t.TempDir(), "final")
	for _, dir := range []string{start, middle, final} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(start, tdRootFile), []byte(middle+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(middle, tdRootFile), []byte(final+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := resolveRoot(start); got != canonicalPath(final) {
		t.Fatalf("resolved %s, want db.Open second-pass root %s", got, final)
	}
}
