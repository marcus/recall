package ranking

import (
	"math/rand"
	"slices"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

// zeroCluster is a cluster that scored zero, which is what every cluster from a
// source reporting zero relevance does: relevance is a factor, so it annihilates
// the rank term and the fused score stops carrying any information.
func zeroCluster(source, local string, rank int, score float64) *cluster {
	basis := recall.Candidate{
		CandidateID: source + ":" + local,
		SourceID:    source,
		SourceUID:   recall.SourceUID(source),
		Locator:     recall.Locator{SourceID: source, Local: local},
		LocalRank:   rank,
	}
	return &cluster{
		primary:    basis,
		scoreBasis: basis,
		score:      score,
		explain: recall.Explanation{
			LineageRoot: recall.LineageRoot(source + ":" + local),
		},
	}
}

// At zero the source's own ordering decides, because nothing else is left. Before
// this, the next comparison was a locator string, so a caller was shown
// alphabetical order presented as a relevance ranking — and on a paraphrase
// query, where every lexical relevance is zero by construction, the correct
// document landed last.
func TestZeroScoreOrdersByLocalRank(t *testing.T) {
	// Deliberately named so that string order and rank order disagree on every
	// pair: alphabetically zulu.md sorts last and it is the source's first hit.
	got := []*cluster{
		zeroCluster("qmd", "alpha.md#L1-L3", 4, 0),
		zeroCluster("qmd", "zulu.md#L1-L3", 1, 0),
		zeroCluster("qmd", "mike.md#L1-L3", 3, 0),
		zeroCluster("qmd", "bravo.md#L1-L3", 2, 0),
	}
	slices.SortStableFunc(got, compareRelevance)

	want := []int{1, 2, 3, 4}
	for i, c := range got {
		if c.scoreBasis.LocalRank != want[i] {
			ranks := make([]int, 0, len(got))
			for _, x := range got {
				ranks = append(ranks, x.scoreBasis.LocalRank)
			}
			t.Fatalf("order = %v, want %v", ranks, want)
		}
	}
}

// The gate is score == 0, not "the two scores are equal". Zero is the only value
// that provably discarded the source's ordering; anything else is a score two
// clusters genuinely tied on, and reordering those would move orderings that are
// not broken. This is what keeps every non-zero baseline byte-identical.
func TestEqualNonZeroScoresStillFallBackToTheLocator(t *testing.T) {
	got := []*cluster{
		zeroCluster("docs", "zulu.md#L1-L3", 1, 0.5),
		zeroCluster("docs", "alpha.md#L1-L3", 9, 0.5),
	}
	slices.SortStableFunc(got, compareRelevance)
	if got[0].scoreBasis.Locator.Local != "alpha.md#L1-L3" {
		t.Fatalf("a non-zero tie was reordered by rank: first = %q",
			got[0].scoreBasis.Locator.Local)
	}
	// And a real score difference still wins over any rank.
	pair := []*cluster{
		zeroCluster("docs", "a.md#L1-L3", 1, 0.1),
		zeroCluster("docs", "b.md#L1-L3", 9, 0.9),
	}
	slices.SortStableFunc(pair, compareRelevance)
	if pair[0].score != 0.9 {
		t.Fatalf("score no longer decides first: %v", pair[0].score)
	}
}

// The comparator must be a strict weak ordering: slices.SortFunc requires it,
// and this comparison's own contract asserts something stronger — that no two
// clusters ever compare equal, so the order cannot depend on which adapter
// answered first.
//
// The cross-source zero ties are the point. A same-source-only rank fallback is
// tempting and is NOT transitive: two clusters from one source at ranks 1 and 2
// plus a third from another source whose locator sorts between them produces a
// cycle. This asserts over a pool built to contain exactly that shape.
func TestCompareRelevanceIsATotalOrder(t *testing.T) {
	pool := []*cluster{
		// One source's ranks in an order that disagrees with string order.
		zeroCluster("qmd", "zulu.md#L1-L3", 1, 0),
		zeroCluster("qmd", "mike.md#L1-L3", 2, 0),
		zeroCluster("qmd", "alpha.md#L1-L3", 3, 0),
		// Another source's locators interleave alphabetically between them,
		// which is the shape a same-source fallback would cycle on.
		zeroCluster("docs", "bravo.md#L1-L3", 1, 0),
		zeroCluster("docs", "november.md#L1-L3", 2, 0),
		// Two sources at the same rank: only the tail can separate these.
		zeroCluster("stream", "alpha.md#L1-L3", 1, 0),
		// Non-zero scores in the same pool, above and below.
		zeroCluster("docs", "yankee.md#L1-L3", 7, 0.4),
		zeroCluster("qmd", "charlie.md#L1-L3", 1, 0.4),
		zeroCluster("docs", "delta.md#L1-L3", 2, 0.9),
	}

	for i, a := range pool {
		for j, b := range pool {
			ab, ba := compareRelevance(a, b), compareRelevance(b, a)
			switch {
			case i == j && ab != 0:
				t.Fatalf("cluster %d does not compare equal to itself: %d", i, ab)
			case i != j && ab == 0:
				t.Fatalf("clusters %d and %d compare equal; the tail is not a unique key", i, j)
			case ab != -ba:
				t.Fatalf("compare(%d,%d) = %d but compare(%d,%d) = %d: not antisymmetric",
					i, j, ab, j, i, ba)
			}
		}
	}
	// Transitivity over every triple. A cycle here is what a source-scoped
	// fallback would have introduced, and slices.SortFunc's behaviour on a
	// non-transitive comparator is unspecified rather than merely wrong.
	for i, a := range pool {
		for j, b := range pool {
			for k, c := range pool {
				if compareRelevance(a, b) < 0 && compareRelevance(b, c) < 0 &&
					compareRelevance(a, c) >= 0 {
					t.Fatalf("compare is not transitive on (%d,%d,%d)", i, j, k)
				}
			}
		}
	}

	// And the order does not depend on the order the adapters answered in.
	sorted := slices.Clone(pool)
	slices.SortStableFunc(sorted, compareRelevance)
	want := make([]string, 0, len(sorted))
	for _, c := range sorted {
		want = append(want, c.scoreBasis.CandidateID)
	}
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 20; trial++ {
		shuffled := slices.Clone(pool)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		slices.SortStableFunc(shuffled, compareRelevance)
		for i, c := range shuffled {
			if c.scoreBasis.CandidateID != want[i] {
				got := make([]string, 0, len(shuffled))
				for _, x := range shuffled {
					got = append(got, x.scoreBasis.CandidateID)
				}
				t.Fatalf("trial %d order = %v, want %v", trial, got, want)
			}
		}
	}
}

// LocalRank is validated at or above one before a group is scored, so the
// comparison is always well defined. Asserted here because the fallback would be
// meaningless if a zero rank could reach it.
func TestZeroScoreFallbackAssumesAValidatedRank(t *testing.T) {
	if got := zeroScoreLocalRank(zeroCluster("a", "a", 1, 0), zeroCluster("b", "b", 2, 0)); got >= 0 {
		t.Fatalf("rank 1 did not sort ahead of rank 2: %d", got)
	}
	if got := zeroScoreLocalRank(zeroCluster("a", "a", 1, 0), zeroCluster("b", "b", 1, 0)); got != 0 {
		t.Fatalf("equal ranks did not defer to the tail: %d", got)
	}
	// One non-zero score is enough to leave the ordering alone.
	for _, pair := range [][2]float64{{0, 0.1}, {0.1, 0}, {0.1, 0.1}} {
		a := zeroCluster("a", "a", 9, pair[0])
		b := zeroCluster("b", "b", 1, pair[1])
		if got := zeroScoreLocalRank(a, b); got != 0 {
			t.Fatalf("scores %v reached the rank fallback: %d", pair, got)
		}
	}
}
