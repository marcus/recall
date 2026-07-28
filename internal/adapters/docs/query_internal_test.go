package docs

import "testing"

// A function word must never be expanded to a number variant of itself.
//
// Scoring runs over the raw query once any content term reaches the index, so
// an expanded "does" would reach "doe" and raise a chunk's score on a corpus
// that has the animal and not the auxiliary — invisibly, because admission,
// relevance, and the excerpt all read the content terms and would never see it.
func TestFunctionWordsAreNeverExpanded(t *testing.T) {
	t.Parallel()
	g := &generation{postings: map[string][]posting{
		"goldeneye": {{chunk: 0, tf: 1}},
		"doe":       {{chunk: 1, tf: 3}},
	}}

	for _, group := range groupTerms(g, []string{"what", "does", "goldeneye", "eat"}) {
		if group.term == "does" && len(group.variants) > 0 {
			t.Errorf("the function word %q was expanded to %v", group.term, group.variants)
		}
	}
	// A content term the corpus does not hold is still expanded, which is the
	// whole rule.
	g.postings["goldeneyes"] = []posting{{chunk: 2, tf: 1}}
	for _, group := range groupTerms(g, []string{"goldeneyes", "eat"}) {
		if group.term == "eat" && len(group.variants) > 0 {
			t.Errorf("a term with no variant in the corpus resolved to %v", group.variants)
		}
	}
}
