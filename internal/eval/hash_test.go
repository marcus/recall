package eval_test

import (
	"strings"
	"testing"

	"github.com/marcus/recall/internal/eval"
)

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
