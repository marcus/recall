package eval_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// The repository carries two packs, and no more without a deliberate change
// here. Authored development questions and source fixtures inherit the source
// corpus's sensitivity and belong behind a user-configured path outside the
// clone; keeping this as an allowlist makes each exception an argument someone
// had to write down rather than an unnoticed copy of private bodies.
//
//   - smoke is synthetic throughout.
//   - firstuse is not. It carries a trimmed, scrubbed snapshot of the
//     configured home corpus, pinned to one commit, because the ranking work it
//     gates has to run in CI and CI has none of those sources. What it holds is
//     documented in its sources/config.toml; what it may not hold is anything
//     the six cases do not need.
func TestRepositoryCarriesOnlyTheAllowedPacks(t *testing.T) {
	root := filepath.Join("..", "..", "eval", "packs")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var packs []string
	for _, entry := range entries {
		if entry.IsDir() {
			packs = append(packs, entry.Name())
		}
	}
	slices.Sort(packs)
	if !slices.Equal(packs, []string{"firstuse", "smoke"}) {
		t.Fatalf("committed packs = %v, want firstuse and smoke; another real "+
			"development pack must stay outside the repository", packs)
	}
}

func TestCommittedEvaluationArtifactsContainNoPersonalAbsolutePaths(t *testing.T) {
	roots := []string{
		filepath.Join("..", "..", "eval", "packs"),
		filepath.Join("..", "..", "eval", "baselines"),
	}
	personalPath := regexp.MustCompile(
		`(?m)(/Users/[^/"\s]+|/home/[^/"\s]+|[A-Za-z]:\\Users\\[^\\/"\s]+)`)

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if match := personalPath.Find(body); match != nil {
				t.Errorf("%s contains a personal absolute path", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
