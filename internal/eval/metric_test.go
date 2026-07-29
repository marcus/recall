package eval_test

import (
	"math"
	"testing"
	"time"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/recall"
)

// roots turns document labels into lineage roots. Metrics key on lineage
// roots, so even a textbook worked example has to be expressed as evidence
// identities rather than document numbers.
func roots(locals ...string) []recall.LineageRoot {
	out := make([]recall.LineageRoot, 0, len(locals))
	for _, l := range locals {
		out = append(out, recall.LineageRoot("uid-src:"+l))
	}
	return out
}

func root(local string) recall.LineageRoot { return recall.LineageRoot("uid-src:" + local) }

func set(locals ...string) eval.RootSet {
	s := make(eval.RootSet, len(locals))
	for _, l := range locals {
		s[root(l)] = true
	}
	return s
}

const tolerance = 1e-9

func closeTo(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %.17g, want %.17g (delta %.3g)", name, got, want, math.Abs(got-want))
	}
}

// round3 rounds for comparison against published figures, which are printed to
// three decimals.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Published worked example: Wikipedia, "Discounted cumulative gain", section
// "Example" — six documents with relevance grades 3, 2, 3, 0, 1, 2, for which
// the article reports DCG = 6.861, ideal DCG = 7.141, and nDCG = 0.961.
//
// This pins the formulation as much as the arithmetic. The article's table
// divides the raw grade by log2(i+1); reproducing its numbers is what proves
// this implementation uses linear gain rather than 2^rel - 1, which is the
// choice that has to match trec_eval later.
func TestNDCGMatchesPublishedWorkedExample(t *testing.T) {
	grades := eval.Grades{
		root("d1"): 3,
		root("d2"): 2,
		root("d3"): 3,
		root("d4"): 0,
		root("d5"): 1,
		root("d6"): 2,
	}
	ranked := roots("d1", "d2", "d3", "d4", "d5", "d6")

	// The published table, term by term.
	wantDCG := 3.0/math.Log2(2) +
		2.0/math.Log2(3) +
		3.0/math.Log2(4) +
		0.0/math.Log2(5) +
		1.0/math.Log2(6) +
		2.0/math.Log2(7)
	// The ideal ordering of the same grades: 3, 3, 2, 2, 1, 0.
	wantIDCG := 3.0/math.Log2(2) +
		3.0/math.Log2(3) +
		2.0/math.Log2(4) +
		2.0/math.Log2(5) +
		1.0/math.Log2(6) +
		0.0/math.Log2(7)

	// If either of these drifts, the formulation stopped matching the published
	// example and every number below would be self-consistent but wrong.
	if got := round3(wantDCG); got != 6.861 {
		t.Fatalf("DCG = %v, published value is 6.861", got)
	}
	if got := round3(wantIDCG); got != 7.141 {
		t.Fatalf("ideal DCG = %v, published value is 7.141", got)
	}

	got, ok := eval.NDCGAt(ranked, grades, 6)
	if !ok {
		t.Fatal("nDCG undefined for a case with positive grades")
	}
	closeTo(t, "nDCG@6", got, wantDCG/wantIDCG)
	if r := round3(got); r != 0.961 {
		t.Errorf("nDCG@6 = %v, published value is 0.961", r)
	}
}

// The cut point must truncate the ideal ranking too. An IDCG taken over more
// documents than the cut allows would make a perfect top-k look imperfect.
func TestNDCGTruncatesTheIdealRankingAtK(t *testing.T) {
	grades := eval.Grades{}
	var ranked []recall.LineageRoot
	for _, l := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		grades[root(l)] = eval.Authoritative
		ranked = append(ranked, root(l))
	}

	got, ok := eval.NDCGAt(ranked, grades, 10)
	if !ok {
		t.Fatal("undefined")
	}
	// Twelve authoritative documents, ten slots, and the ten best are in the ten
	// slots. That is a perfect ranking at this cut point.
	closeTo(t, "nDCG@10", got, 1.0)
}

// A case with nothing positive to find has no ideal ranking to be normalized
// against. Scoring it zero would count a correct abstention as a total failure
// and drag every average down with it.
func TestNDCGUndefinedWithoutPositiveGrades(t *testing.T) {
	grades := eval.Grades{root("d1"): eval.Irrelevant}
	if _, ok := eval.NDCGAt(roots("d1"), grades, 10); ok {
		t.Error("nDCG defined for a case with no positive grade")
	}
}

// Published worked example: Wikipedia, "Precision and recall", section
// "Definition" — a detector returns 8 items, 5 of which are among the 12 that
// should have been found, giving recall 5/12.
func TestRecallAtKMatchesPublishedWorkedExample(t *testing.T) {
	want := set("t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12")
	// Eight returned: five of the twelve, three that are not.
	ranked := roots("t1", "f1", "t2", "f2", "t3", "t4", "f3", "t5")

	got, ok := eval.RecallAt(ranked, want, 8)
	if !ok {
		t.Fatal("recall undefined with a non-empty target set")
	}
	closeTo(t, "Recall@8", got, 5.0/12.0)
}

// Two projections of one record are one piece of evidence. Counting a repeat
// would reward a ranking that failed to deduplicate.
func TestRecallCountsARepeatedRootOnce(t *testing.T) {
	want := set("a", "b")
	ranked := roots("a", "a", "a", "a", "a")

	got, ok := eval.RecallAt(ranked, want, 5)
	if !ok {
		t.Fatal("undefined")
	}
	closeTo(t, "Recall@5", got, 0.5)
}

func TestRecallUndefinedWhenNothingWasRequired(t *testing.T) {
	if _, ok := eval.RecallAt(roots("a"), eval.RootSet{}, 5); ok {
		t.Error("recall defined with nothing to find")
	}
}

// Published worked example: Wikipedia, "Mean reciprocal rank", section
// "Example" — three queries whose correct answers rank 3rd, 2nd, and 1st,
// giving MRR = (1/3 + 1/2 + 1)/3 = 11/18 ≈ 0.61.
func TestMRRMatchesPublishedWorkedExample(t *testing.T) {
	queries := []struct {
		ranked  []recall.LineageRoot
		correct string
		wantRR  float64
	}{
		{roots("catten", "cati", "cats"), "cats", 1.0 / 3.0},
		{roots("torii", "tori", "toruses"), "tori", 1.0 / 2.0},
		{roots("viruses", "virii", "viri"), "viruses", 1.0},
	}

	sum := 0.0
	for _, q := range queries {
		got, ok := eval.MRRAt(q.ranked, set(q.correct), 10)
		if !ok {
			t.Fatalf("%q: MRR undefined with a known answer", q.correct)
		}
		closeTo(t, "RR for "+q.correct, got, q.wantRR)
		sum += got
	}

	mrr := sum / float64(len(queries))
	closeTo(t, "MRR", mrr, 11.0/18.0)
	if r := math.Round(mrr*100) / 100; r != 0.61 {
		t.Errorf("MRR = %v, published value is 0.61", r)
	}
}

// A miss is zero, not undefined: the evidence existed and the ranking did not
// surface it. Only a case with nothing authoritative to find is undefined.
func TestMRRDistinguishesAMissFromNothingToFind(t *testing.T) {
	got, ok := eval.MRRAt(roots("x", "y"), set("z"), 10)
	if !ok {
		t.Fatal("MRR undefined although the case had an answer to find")
	}
	closeTo(t, "MRR@10", got, 0)

	if _, ok := eval.MRRAt(roots("x"), eval.RootSet{}, 10); ok {
		t.Error("MRR defined with nothing to find")
	}
}

// Beyond the cut point is a miss, however good the result is at rank 11.
func TestMRRHonorsTheCutPoint(t *testing.T) {
	ranked := roots("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "target")
	got, ok := eval.MRRAt(ranked, set("target"), 10)
	if !ok {
		t.Fatal("undefined")
	}
	closeTo(t, "MRR@10", got, 0)
}

func TestHitAt(t *testing.T) {
	cases := []struct {
		name   string
		ranked []recall.LineageRoot
		want   eval.RootSet
		k      int
		hit    bool
	}{
		{"inside the cut", roots("a", "b", "c"), set("c"), 5, true},
		{"outside the cut", roots("a", "b", "c"), set("c"), 2, false},
		{"nothing to hit", roots("a"), eval.RootSet{}, 5, false},
		{"k of zero hits nothing", roots("a"), set("a"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eval.HitAt(tc.ranked, tc.want, tc.k); got != tc.hit {
				t.Errorf("HitAt = %v, want %v", got, tc.hit)
			}
		})
	}
}

// Published worked example: Wikipedia, "Percentile", section "The nearest-rank
// method" — for the ordered list 15, 20, 35, 40, 50 the article gives
// P5 = 15, P30 = 20, P40 = 20, P50 = 35, and P100 = 50.
//
// Nearest rank matters for a latency budget: an interpolated p95 reports a
// duration no request ever took, and a budget should be held against something
// that actually happened.
func TestPercentileMatchesPublishedWorkedExample(t *testing.T) {
	// Deliberately unsorted: percentiles must not depend on arrival order.
	sample := []time.Duration{40, 15, 50, 20, 35}

	cases := []struct {
		p    float64
		want time.Duration
	}{
		{5, 15},
		{30, 20},
		{40, 20},
		{50, 35},
		{100, 50},
	}
	for _, tc := range cases {
		got, ok := eval.Percentile(sample, tc.p)
		if !ok {
			t.Fatalf("P%v undefined", tc.p)
		}
		if got != tc.want {
			t.Errorf("P%v = %v, published value is %v", tc.p, got, tc.want)
		}
	}

	// The caller's slice is an observation record, not scratch space.
	if sample[0] != 40 {
		t.Errorf("Percentile reordered the caller's slice: %v", sample)
	}
}

func TestPercentileUndefinedForAnEmptySample(t *testing.T) {
	if _, ok := eval.Percentile(nil, 95); ok {
		t.Error("percentile defined over no observations")
	}
}
