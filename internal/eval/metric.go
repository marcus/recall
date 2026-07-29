package eval

import (
	"math"
	"sort"
	"time"

	"github.com/marcus/recall/pkg/recall"
)

// Grades is one case's judged relevance, keyed by lineage root. A root absent
// from the map is unjudged and contributes gain 0 — unjudged is not the same
// claim as judged irrelevant, but for a graded sum it has the same weight.
type Grades map[recall.LineageRoot]Relevance

// RootSet is a set of lineage roots: required evidence, forbidden evidence, or
// whatever else a metric is measured against.
type RootSet map[recall.LineageRoot]bool

// Value is one metric for one case. OK reports whether the metric applies.
//
// Undefined is not zero. A case with no forbidden evidence cannot score 0 on
// Forbidden@5 without diluting the rate over cases that were never at risk,
// and a case with nothing authoritative to find cannot score 0 on MRR@10
// without inventing a regression. Averaging must skip these, not count them.
type Value struct {
	V  float64 `json:"value"`
	OK bool    `json:"defined"`
}

func defined(v float64) Value { return Value{V: v, OK: true} }

func boolValue(b bool) Value {
	if b {
		return defined(1)
	}
	return defined(0)
}

// NDCGAt returns normalized discounted cumulative gain over the first k
// positions of ranked.
//
// Formulation, stated because both common conventions are called "nDCG":
//
//	DCG@k  = sum over i in 1..k of  rel_i / log2(i + 1)
//	IDCG@k = the same sum over the judged grades in descending order
//	nDCG@k = DCG@k / IDCG@k
//
// Gain is linear in the grade and the discount is log2(i+1) with one-based i.
// That is the formulation in the standard published worked example this
// implementation is verified against, and it is trec_eval's default gain.
// docs/evaluation.md commits to cross-checking metric implementations against
// trec_eval in stage 2, so matching it is not a stylistic choice; the stage-2
// cross-check is what will confirm the match rather than assume it. The other
// convention, exponential gain (2^rel - 1), is deliberately not used here: it
// would make every number incomparable with that cross-check.
//
// The result is undefined when no judged grade is positive: with IDCG of zero
// there is nothing a ranking could have done, and scoring it 0 would punish a
// perfect abstention.
func NDCGAt(ranked []recall.LineageRoot, grades Grades, k int) (float64, bool) {
	if k <= 0 {
		return 0, false
	}

	dcg := 0.0
	for i, root := range ranked {
		if i >= k {
			break
		}
		if g := grades[root]; g > Irrelevant {
			dcg += float64(g) / math.Log2(float64(i+2))
		}
	}

	ideal := make([]Relevance, 0, len(grades))
	for _, g := range grades {
		if g > Irrelevant {
			ideal = append(ideal, g)
		}
	}
	sort.Slice(ideal, func(i, j int) bool { return ideal[i] > ideal[j] })

	idcg := 0.0
	for i, g := range ideal {
		if i >= k {
			break
		}
		idcg += float64(g) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0, false
	}
	return dcg / idcg, true
}

// RecallAt returns the fraction of want found in the first k positions.
//
// A root appearing twice in the ranking counts once: two projections of one
// record are one piece of evidence, so rewarding the repeat would reward
// failing to deduplicate.
//
// The result is undefined when want is empty — there was nothing to find.
func RecallAt(ranked []recall.LineageRoot, want RootSet, k int) (float64, bool) {
	if k <= 0 || len(want) == 0 {
		return 0, false
	}
	found := make(RootSet, len(want))
	for i, root := range ranked {
		if i >= k {
			break
		}
		if want[root] {
			found[root] = true
		}
	}
	return float64(len(found)) / float64(len(want)), true
}

// MRRAt returns the reciprocal of the rank of the first root in want, or 0
// when none appears in the first k positions.
//
// The result is undefined when want is empty. It is defined, and zero, when
// want is non-empty and nothing was found: that is a real miss.
func MRRAt(ranked []recall.LineageRoot, want RootSet, k int) (float64, bool) {
	if k <= 0 || len(want) == 0 {
		return 0, false
	}
	for i, root := range ranked {
		if i >= k {
			break
		}
		if want[root] {
			return 1 / float64(i+1), true
		}
	}
	return 0, true
}

// HitAt reports whether any root in want appears in the first k positions. It
// is the shared shape of Success@k and Forbidden@k, which differ only in what
// they are looking for and in which direction is good.
func HitAt(ranked []recall.LineageRoot, want RootSet, k int) bool {
	if k <= 0 {
		return false
	}
	for i, root := range ranked {
		if i >= k {
			break
		}
		if want[root] {
			return true
		}
	}
	return false
}

// Percentile returns the nearest-rank percentile of d, in the range (0, 100].
//
// Nearest rank means the value at position ceil(p/100 * n) of the sorted
// sample, with no interpolation: a reported p95 is a latency that was actually
// observed. Interpolating would report a number no request ever took, which is
// the wrong thing to hold a budget against.
//
// d is not modified.
func Percentile(d []time.Duration, p float64) (time.Duration, bool) {
	if len(d) == 0 || p <= 0 || p > 100 {
		return 0, false
	}
	sorted := make([]time.Duration, len(d))
	copy(sorted, d)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Multiply before dividing: p*n is exact for the integral percentiles that
	// matter, where p*n/100 computed the other way is not.
	rank := int(math.Ceil(p * float64(len(sorted)) / 100))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1], true
}
