package recall_test

import (
	"math"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

// The two factors exist because each one alone gets a case in the eval packs
// wrong. These are those cases, in miniature.
func TestRelevanceSeparatesWhatEachFactorAloneCannot(t *testing.T) {
	// dentist-001: a one-word query. Both candidates cover it completely, so
	// coverage alone ties them and concentration is the whole difference.
	task := recall.Relevance(1, 1, 1, 4)    // "Make a dentist appointment"
	chunk := recall.Relevance(1, 1, 1, 180) // a chunk using the word in an aside
	if !(task > chunk) {
		t.Errorf("task %v, chunk %v: a record that IS the query must beat one that mentions it", task, chunk)
	}
	if ratio := task / chunk; ratio < 3 {
		t.Errorf("ratio %v: the separation must be decisive enough to overcome a source prior", ratio)
	}

	// The natural-language flood: a short record matching one common term of
	// six looks concentrated, and only coverage says it is not an answer.
	oneOfSix := recall.Relevance(1, 6, 1, 12)
	sixOfSix := recall.Relevance(6, 6, 6, 12)
	if !(sixOfSix > oneOfSix*4) {
		t.Errorf("one-of-six %v, six-of-six %v: coverage must dominate a partial match",
			oneOfSix, sixOfSix)
	}
}

func TestRelevanceIsBoundedAndTotal(t *testing.T) {
	cases := []struct{ matched, retained, hits, length int }{
		{0, 0, 0, 0}, {1, 1, 1, 1}, {5, 2, 100, 1}, {0, 5, 0, 100},
		{3, 3, 3, 3}, {1, 4, 2, 1000}, {2, 2, 0, 50},
	}
	for _, c := range cases {
		got := recall.Relevance(c.matched, c.retained, c.hits, c.length)
		if math.IsNaN(got) || got < 0 || got > 1 {
			t.Errorf("Relevance(%d,%d,%d,%d) = %v, want [0,1]",
				c.matched, c.retained, c.hits, c.length, got)
		}
	}
}

// A browse — no query — is not "relevant to nothing". Every record is equally
// relevant to no question, and returning 0 would silently score a whole source
// out of the answer.
func TestRelevanceOfABrowseIsOne(t *testing.T) {
	if got := recall.Relevance(0, 0, 0, 40); got != 1 {
		t.Errorf("no-query relevance = %v, want 1", got)
	}
	if got := recall.Relevance(2, 2, 2, 0); got != 1 {
		t.Errorf("unknown-length relevance = %v, want 1: guessing low would demote a "+
			"source that failed to report a length as if it had matched badly", got)
	}
}

// Concentration is a half at the reference density, by construction. This pins
// the constant to its stated meaning, so moving it is a deliberate act.
func TestConcentrationIsAHalfAtTheReference(t *testing.T) {
	length := 100
	hits := int(recall.ConcentrationReference * float64(length)) // 1 in 50 -> 2 in 100
	got := recall.Relevance(1, 1, hits, length)
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("relevance at the reference density = %v, want 0.5", got)
	}
}
