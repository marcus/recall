package recall_test

import (
	"slices"
	"testing"

	"github.com/marcus/recall/internal/recall"
)

// The reported failure, three for three: a singular query term against a corpus
// that spells the word in the plural. Each of these abstained before the rule
// existed, on a twelve-record store that held the answer.
func TestNumberVariantsReachTheReportedPlurals(t *testing.T) {
	t.Parallel()
	cases := []struct{ query, corpus string }{
		{"goldeneye", "goldeneyes"},
		{"bufflehead", "buffleheads"},
		{"merganser", "mergansers"},
		{"goldeneyes", "goldeneye"},
		{"index", "indexes"},
		{"cache", "caches"},
		{"cities", "city"},
		{"city", "cities"},
		{"batch", "batches"},
		{"classes", "class"},
	}
	for _, c := range cases {
		if got := recall.NumberVariants(c.query); !slices.Contains(got, c.corpus) {
			t.Errorf("NumberVariants(%q) = %v, missing %q", c.query, got, c.corpus)
		}
	}
}

// The precision half. Every pair here is two different words, and a rule that
// merged them would spend the abstention property it was written to protect.
func TestNumberVariantsRefuseFalseMerges(t *testing.T) {
	t.Parallel()
	cases := []struct{ query, unwanted string }{
		{"universal", "universe"},    // the stemmer failure this rule avoids
		{"as", "ass"},                // two letters derive nothing at all
		{"us", "use"},                //
		{"notes", "not"},             // -es only strips after a sibilant
		{"class", "class"[:4]},       // -ss is not a plural marker, and the stem is not a word
		{"less", "less"[:3]},         //
		{"was", "wa"},                // a two-letter singular is not derived
		{"his", "hi"},                //
		{"analysis", "analysi"},      // len 7, but the derivation is still wrong
		{"bus", "bu"},                //
		{"series", "serie"},          // -ies wins over -s
		{"identifiers", "identifie"}, // -s strips exactly one letter
	}
	for _, c := range cases {
		if got := recall.NumberVariants(c.query); slices.Contains(got, c.unwanted) {
			t.Errorf("NumberVariants(%q) = %v, must not contain %q", c.query, got, c.unwanted)
		}
	}
}

// A variant is never the term itself, and never repeats: both would double a
// term's weight against a corpus that happens to spell it one way.
func TestNumberVariantsAreDistinct(t *testing.T) {
	t.Parallel()
	for _, term := range []string{"index", "city", "cities", "boxes", "note", "notes", "recall"} {
		seen := map[string]bool{term: true}
		for _, v := range recall.NumberVariants(term) {
			if seen[v] {
				t.Errorf("NumberVariants(%q) repeats %q", term, v)
			}
			seen[v] = true
		}
	}
}

// The variant only ever resolves into a vocabulary the source holds. A
// generated spelling nobody wrote must not become a term.
func TestVariantsInReadsTheSourcesVocabulary(t *testing.T) {
	t.Parallel()
	vocab := map[string]bool{"goldeneyes": true, "mergansers": true}
	got := recall.VariantsIn("goldeneye", func(s string) bool { return vocab[s] })
	if len(got) != 1 || got[0] != "goldeneyes" {
		t.Fatalf("VariantsIn = %v, want [goldeneyes]", got)
	}
	if got := recall.VariantsIn("bufflehead", func(s string) bool { return vocab[s] }); len(got) != 0 {
		t.Errorf("VariantsIn over a vocabulary without the word = %v, want none", got)
	}
}

// A term the source already spells the caller's way is not expanded at all.
// This is the gate that keeps a common noun from reaching further: expanding
// "project" against a corpus that holds it would admit every record whose only
// claim is the word "projects".
func TestResolveTermVariantsExpandsOnlyWhatTheSourceLacks(t *testing.T) {
	t.Parallel()
	vocab := map[string]bool{"project": true, "projects": true, "goldeneyes": true}
	holds := func(s string) bool { return vocab[s] }

	got := recall.ResolveTermVariants([]string{"project", "goldeneye", "fujifilm"}, holds)
	if _, expanded := got["project"]; expanded {
		t.Errorf("a term the source holds was expanded to %v", got["project"])
	}
	if want := []string{"goldeneyes"}; !slices.Equal(got["goldeneye"], want) {
		t.Errorf("goldeneye resolved to %v, want %v", got["goldeneye"], want)
	}
	if _, expanded := got["fujifilm"]; expanded {
		t.Errorf("a term no spelling of which is in the source was expanded to %v", got["fujifilm"])
	}
}

// The caller's own spelling wins its own query: a variant is worth half.
func TestWeighPrefersTheSpellingThatWasTyped(t *testing.T) {
	t.Parallel()
	resolved := recall.TermVariants{"goldeneye": {"goldeneyes"}}
	if got := resolved.Weigh(map[string]float64{"goldeneye": 1, "goldeneyes": 1}, "goldeneye"); got != 1 {
		t.Errorf("own spelling weighed %v, want 1", got)
	}
	if got := resolved.Weigh(map[string]float64{"goldeneyes": 1}, "goldeneye"); got != recall.NumberVariantWeight {
		t.Errorf("variant weighed %v, want %v", got, recall.NumberVariantWeight)
	}
	if got := resolved.Weigh(map[string]float64{"goldeneyes": 1}, "fujifilm"); got != 0 {
		t.Errorf("unresolved term weighed %v, want 0", got)
	}
	// A source that resolved nothing weighs exactly what it always did.
	if got := recall.TermVariants(nil).Weigh(map[string]float64{"goldeneyes": 1}, "goldeneye"); got != 0 {
		t.Errorf("unresolved variant weighed %v, want 0", got)
	}
}

// Relevance counts a resolved variant at full weight: a plural is exactly as
// much ABOUT the query as the singular, and relevance is compared across
// sources.
func TestRelevanceOverCountsIsNumberBlindOnceResolved(t *testing.T) {
	t.Parallel()
	resolved := recall.TermVariants{"goldeneye": {"goldeneyes"}}
	singular := resolved.RelevanceOverCounts([]string{"goldeneye"}, map[string]int{"goldeneye": 2}, 40)
	plural := resolved.RelevanceOverCounts([]string{"goldeneye"}, map[string]int{"goldeneyes": 2}, 40)
	if singular != plural {
		t.Errorf("relevance %v under the query's spelling, %v under the corpus's; want the same", singular, plural)
	}
	if plural == 0 {
		t.Fatal("a record spelling the term in the plural measured as not about the query at all")
	}
	// The package-level function is the unresolved case, unchanged.
	if got := recall.RelevanceOverCounts([]string{"goldeneye"}, map[string]int{"goldeneyes": 2}, 40); got != 0 {
		t.Errorf("unresolved relevance = %v, want 0", got)
	}
}

// Findings from the independent review of this rule, each one an input the
// original tests did not cover.
func TestNumberVariantsRefuseTheReviewedFalseMerges(t *testing.T) {
	t.Parallel()
	// "lens" is not a plural, and "len" is a word every corpus near code holds.
	if got := recall.NumberVariants("lens"); slices.Contains(got, "len") {
		t.Errorf("NumberVariants(\"lens\") = %v, must not reach a length", got)
	}
	if got := recall.NumberVariants("news"); slices.Contains(got, "new") {
		t.Errorf("NumberVariants(\"news\") = %v, must not reach \"new\"", got)
	}
	// A short -ies word is an -s plural of an -ie word, not a -y one.
	for _, tc := range []struct{ query, want, unwanted string }{
		{"ties", "tie", "ty"},
		{"lies", "lie", "ly"},
		{"dies", "die", "dy"},
	} {
		got := recall.NumberVariants(tc.query)
		if !slices.Contains(got, tc.want) {
			t.Errorf("NumberVariants(%q) = %v, missing %q", tc.query, got, tc.want)
		}
		if slices.Contains(got, tc.unwanted) {
			t.Errorf("NumberVariants(%q) = %v, must not contain %q", tc.query, got, tc.unwanted)
		}
	}
	// The -y rule still applies where the word is long enough to be one.
	if got := recall.NumberVariants("cities"); !slices.Contains(got, "city") {
		t.Errorf("NumberVariants(\"cities\") = %v, missing \"city\"", got)
	}
}

// A term repeated in a query is probed once: holds() is a scan of a source's
// whole vocabulary, and walking it twice for one answer is pure cost.
func TestResolveTermVariantsProbesARepeatedTermOnce(t *testing.T) {
	t.Parallel()
	probes := 0
	holds := func(string) bool { probes++; return false }
	recall.ResolveTermVariants([]string{"zxqv", "zxqv", "zxqv"}, holds)
	first := probes

	probes = 0
	recall.ResolveTermVariants([]string{"zxqv"}, holds)
	if probes != first {
		t.Errorf("three copies of one term cost %d probes, one costs %d", first, probes)
	}
}
