package eval_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/eval"
)

func fixturePack(
	t *testing.T,
	sourceFiles map[string]string,
	transcriptFiles map[string]string,
) (*eval.Pack, []eval.Case, []eval.Judgment, string) {
	t.Helper()
	manifest := strings.Replace(
		manifestJSON,
		`"judgments": "judgments.jsonl",`,
		`"judgments": "judgments.jsonl",
  "sources": "sources",
  "transcripts": "transcripts",`,
		1,
	)
	dir := writePack(t, map[string]string{eval.PackFile: manifest})
	writeTree := func(root string, files map[string]string) {
		if err := os.MkdirAll(filepath.Join(dir, root), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			path := filepath.Join(dir, root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeTree("sources", sourceFiles)
	writeTree("transcripts", transcriptFiles)

	pack, err := eval.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := pack.LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	judgments, err := pack.LoadJudgments()
	if err != nil {
		t.Fatal(err)
	}
	return pack, cases, judgments, dir
}

func hashOf(t *testing.T, p *eval.Pack, cases []eval.Case, judgments []eval.Judgment) string {
	t.Helper()
	h, err := eval.ContentHash(p, cases, judgments)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A pack's identity is what it says, not what order it says it in. Sorting a
// case file, or appending a new case rather than inserting it, must not make a
// run incomparable with the baseline it should be compared against.
func TestContentHashIsStableAcrossOrdering(t *testing.T) {
	p := goodPack()
	cases := []eval.Case{goodCase("a"), goodCase("b"), goodCase("c")}
	judgments := []eval.Judgment{
		goodJudgment("a", "uid:1"),
		goodJudgment("b", "uid:2"),
		goodJudgment("c", "uid:3"),
	}

	want := hashOf(t, p, cases, judgments)

	shuffledCases := []eval.Case{cases[2], cases[0], cases[1]}
	shuffledJudgments := []eval.Judgment{judgments[1], judgments[2], judgments[0]}
	if got := hashOf(t, p, shuffledCases, shuffledJudgments); got != want {
		t.Errorf("reordering changed the hash:\n%s\n%s", want, got)
	}
}

func TestContentHashChangesWithContent(t *testing.T) {
	p := goodPack()
	cases := []eval.Case{goodCase("a")}
	judgments := []eval.Judgment{goodJudgment("a", "uid:1")}
	base := hashOf(t, p, cases, judgments)

	changes := []struct {
		name      string
		pack      *eval.Pack
		cases     []eval.Case
		judgments []eval.Judgment
	}{
		{"a case added", p, append(cases, goodCase("b")), judgments},
		{"a judgment added", p, cases, append(judgments, goodJudgment("a", "uid:2"))},
		{"a grade changed", p, cases, []eval.Judgment{{
			SchemaVersion: 1, CaseID: "a", LineageRoot: "uid:1", Relevance: eval.UsefulSupport, Required: true,
		}}},
		{"a query reworded", p, []eval.Case{func() eval.Case {
			c := goodCase("a")
			c.Query = "a different question"
			return c
		}()}, judgments},
		{"a threshold changed", func() *eval.Pack {
			q := goodPack()
			q.Thresholds = map[string]float64{"abstention_accuracy": 0.95}
			return q
		}(), cases, judgments},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			if got := hashOf(t, tc.pack, tc.cases, tc.judgments); got == base {
				t.Error("hash did not change")
			}
		})
	}
}

func TestContentHashCoversFixturePathsAndBytes(t *testing.T) {
	files := map[string]string{
		"alpha.txt":      "alpha\n",
		"nested/beta.md": "beta\n",
	}
	transcripts := map[string]string{"denied/response.jsonl": `{"outcome":"denied"}` + "\n"}
	pack, cases, judgments, _ := fixturePack(t, files, transcripts)
	base := hashOf(t, pack, cases, judgments)

	same, sameCases, sameJudgments, _ := fixturePack(t, map[string]string{
		"nested/beta.md": "beta\n",
		"alpha.txt":      "alpha\n",
	}, transcripts)
	if got := hashOf(t, same, sameCases, sameJudgments); got != base {
		t.Errorf("creation order or absolute pack path changed fixture identity:\n%s\n%s", base, got)
	}

	changedBytes, byteCases, byteJudgments, _ := fixturePack(t, map[string]string{
		"alpha.txt":      "changed\n",
		"nested/beta.md": "beta\n",
	}, transcripts)
	if got := hashOf(t, changedBytes, byteCases, byteJudgments); got == base {
		t.Error("editing source fixture bytes did not change the hash")
	}

	changedPath, pathCases, pathJudgments, _ := fixturePack(t, map[string]string{
		"renamed-alpha.txt": "alpha\n",
		"nested/beta.md":    "beta\n",
	}, transcripts)
	if got := hashOf(t, changedPath, pathCases, pathJudgments); got == base {
		t.Error("renaming a source fixture did not change the hash")
	}

	changedTranscript, transcriptCases, transcriptJudgments, _ := fixturePack(
		t, files, map[string]string{"denied/response.jsonl": `{"outcome":"success"}` + "\n"})
	if got := hashOf(t, changedTranscript, transcriptCases, transcriptJudgments); got == base {
		t.Error("editing a replay transcript did not change the hash")
	}
}

func TestContentHashIgnoresFixtureMetadataAndEmptyDirectories(t *testing.T) {
	pack, cases, judgments, dir := fixturePack(
		t, map[string]string{"alpha.txt": "alpha\n"}, nil)
	base := hashOf(t, pack, cases, judgments)

	path := filepath.Join(dir, "sources", "alpha.txt")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sources", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := hashOf(t, pack, cases, judgments); got != base {
		t.Errorf("portable-irrelevant metadata changed the hash:\n%s\n%s", base, got)
	}
}

func TestContentHashRejectsUnsafeOrMissingFixtureTrees(t *testing.T) {
	t.Run("missing declared tree", func(t *testing.T) {
		pack, cases, judgments, dir := fixturePack(t, nil, nil)
		if err := os.Remove(filepath.Join(dir, "sources")); err != nil {
			t.Fatal(err)
		}
		if _, err := eval.ContentHash(pack, cases, judgments); err == nil {
			t.Fatal("a missing declared sources tree was accepted")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		pack, cases, judgments, dir := fixturePack(
			t, map[string]string{"alpha.txt": "alpha\n"}, nil)
		target := filepath.Join(dir, "outside.txt")
		if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "sources", "escape")); err != nil {
			t.Skipf("cannot create symlink on this platform: %v", err)
		}
		_, err := eval.ContentHash(pack, cases, judgments)
		if !errors.Is(err, eval.ErrUnsafeFixtureTree) {
			t.Fatalf("err = %v, want ErrUnsafeFixtureTree", err)
		}
	})

	t.Run("nested fixture root through intermediate symlink", func(t *testing.T) {
		pack, cases, judgments, dir := fixturePack(t, nil, nil)
		outside := t.TempDir()
		if err := os.Mkdir(filepath.Join(outside, "docs"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(outside, "docs", "external.md"), []byte("outside\n"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "fixtures")); err != nil {
			t.Skipf("cannot create symlink on this platform: %v", err)
		}
		pack.Sources = "fixtures/docs"

		_, err := eval.ContentHash(pack, cases, judgments)
		if !errors.Is(err, eval.ErrUnsafeFixtureTree) {
			t.Fatalf("err = %v, want ErrUnsafeFixtureTree", err)
		}
		if err != nil && !strings.Contains(err.Error(), "fixtures") {
			t.Errorf("error does not name the intermediate component: %v", err)
		}
	})

	t.Run("path traversal", func(t *testing.T) {
		pack, cases, judgments, _ := fixturePack(t, nil, nil)
		pack.Sources = "../outside"
		_, err := eval.ContentHash(pack, cases, judgments)
		if !errors.Is(err, eval.ErrUnsafePackPath) {
			t.Fatalf("err = %v, want ErrUnsafePackPath", err)
		}
	})
}

func TestFixtureEditMakesRunsNotComparable(t *testing.T) {
	pack, cases, judgments, dir := fixturePack(
		t, map[string]string{"corpus.md": "before\n"}, nil)
	before := hashOf(t, pack, cases, judgments)
	if err := os.WriteFile(
		filepath.Join(dir, "sources", "corpus.md"), []byte("after\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	after := hashOf(t, pack, cases, judgments)

	comparison := eval.Compare(
		eval.Run{RunID: "baseline", Status: eval.StatusPass, Pack: eval.PackRef{ContentHash: before}},
		eval.Run{RunID: "candidate", Status: eval.StatusPass, Pack: eval.PackRef{ContentHash: after}},
		nil,
		nil,
	)
	if comparison.Comparable {
		t.Fatal("a source fixture edit remained comparable to the old baseline")
	}
	if len(comparison.Blockers) == 0 || !strings.Contains(comparison.Blockers[0], "pack content differs") {
		t.Errorf("comparison did not explain the fixture invalidation: %v", comparison.Blockers)
	}
}

// Grading one thing twice is a different pack from grading it once, even
// though sorting the digests would happily collapse a set.
func TestContentHashCountsDuplicates(t *testing.T) {
	p := goodPack()
	cases := []eval.Case{goodCase("a")}
	one := []eval.Judgment{goodJudgment("a", "uid:1")}
	two := []eval.Judgment{goodJudgment("a", "uid:1"), goodJudgment("a", "uid:1")}

	if hashOf(t, p, cases, one) == hashOf(t, p, cases, two) {
		t.Error("a duplicated judgment hashed the same as one judgment")
	}
}

// Where a pack sits on disk is not part of what it says. A pack copied to
// another machine must compare equal to itself.
func TestContentHashIgnoresWhereThePackLives(t *testing.T) {
	fromDisk, err := eval.LoadPack(writePack(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := fromDisk.LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	judgments, err := fromDisk.LoadJudgments()
	if err != nil {
		t.Fatal(err)
	}

	elsewhere, err := eval.LoadPack(writePack(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if fromDisk.Dir() == elsewhere.Dir() {
		t.Fatal("both packs landed in one directory; the test proves nothing")
	}
	if hashOf(t, fromDisk, cases, judgments) != hashOf(t, elsewhere, cases, judgments) {
		t.Error("the pack's directory leaked into its content hash")
	}
}

func TestContentHashIsLabelled(t *testing.T) {
	h := hashOf(t, goodPack(), nil, nil)
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Errorf("hash = %q, want sha256:<64 hex>", h)
	}
}
