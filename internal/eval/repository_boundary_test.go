package eval_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// The repository carries two synthetic packs, and no more without a deliberate
// change here. Authored development questions and source fixtures inherit the
// source corpus's sensitivity and belong behind a user-configured path outside
// the clone. The allowlist makes any new committed corpus an explicit review.
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
	if !slices.Equal(packs, []string{"shapes", "smoke"}) {
		t.Fatalf("committed packs = %v, want shapes and smoke; real development "+
			"packs must stay outside the repository", packs)
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
