package recall

// Relevance is the one definition of [Candidate.Relevance], kept here rather
// than in each adapter because a number is only comparable across sources if
// every source computes it the same way. An adapter that needs a different
// rule needs a different field, not a different formula behind this one.

// ConcentrationReference is the term density at which concentration reaches
// one half: one matched occurrence in fifty terms.
//
// It is a reference point, not a threshold — nothing is admitted or dropped by
// it. It sets how fast a record stops counting as being about a term as the
// record gets longer, and 1-in-50 is roughly a mention per paragraph, the
// density at which a human stops calling a document "about" a word.
//
// The value is stated once, here, because it is a ranking choice: an evaluation
// pack that moves when it moves is measuring this constant, and a constant
// buried in an adapter would make that movement look like a bug in the adapter.
const ConcentrationReference = 0.02

// Relevance combines query coverage with match concentration into [0,1].
//
//	matched   distinct retained query terms this record matched
//	retained  distinct retained query terms in the query
//	hits      matched term occurrences in the record
//	length    the record's length in terms
//
// A query with no retained terms is a browse rather than a search: there is no
// query to be about, so every record is equally relevant to it and the answer
// is 1. A record with no length cannot be reasoned about and gets the same
// answer, because guessing low would silently demote a source that failed to
// report a length rather than one that matched badly.
func Relevance(matched, retained, hits, length int) float64 {
	if retained <= 0 || length <= 0 {
		return 1
	}
	if matched <= 0 || hits <= 0 {
		return 0
	}

	coverage := float64(matched) / float64(retained)
	if coverage > 1 {
		coverage = 1
	}

	// Saturating rather than linear: density is unbounded above in principle
	// (a one-word record matching one term has density 1), and a linear scale
	// would let a very short record dominate a merely good one by an arbitrary
	// factor. d/(d+r) is 0.5 at the reference, approaches 1, never reaches it.
	density := float64(hits) / float64(length)
	concentration := density / (density + ConcentrationReference)

	return coverage * concentration
}

// RelevanceOverCounts is [Relevance] for a source that already holds a token
// count map for the record — which is every source that builds an index.
//
// It exists so the loop that turns "a map of token counts" into the four
// numbers is written once. Four adapters had written it identically, and the
// browse case had picked up two different spellings across them: three guarded
// `len(terms) == 0` themselves and two relied on [Relevance] to return 1. One
// definition means one answer.
func RelevanceOverCounts(terms []string, counts map[string]int, length int) float64 {
	covered, hits := 0, 0
	for _, term := range terms {
		if n := counts[term]; n > 0 {
			covered++
			hits += n
		}
	}
	return Relevance(covered, len(terms), hits, length)
}

// CountTerms is the length of text in whitespace-separated terms, without the
// slice [strings.Fields] would allocate to answer the same question. Record
// text runs to kilobytes in some sources, and this is called once per
// candidate.
func CountTerms(text string) int {
	n, inTerm := 0, false
	for _, r := range text {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			inTerm = false
		case !inTerm:
			inTerm = true
			n++
		}
	}
	return n
}
