package eval_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// The repository carries one tiny, synthetic pack for CI wiring. Authored
// development questions and source fixtures inherit the source corpus's
// sensitivity and must live behind a user-configured path outside the clone.
// Keeping this as an allowlist makes adding another committed pack a deliberate
// test change, not an unnoticed copy of private bodies.
func TestRepositoryCarriesOnlyTheSyntheticSmokePack(t *testing.T) {
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
	if !slices.Equal(packs, []string{"smoke"}) {
		t.Fatalf("committed packs = %v, want only synthetic smoke; real development "+
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
