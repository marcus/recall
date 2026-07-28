package docs

import (
	"slices"
	"strings"
	"testing"
)

// The reported case: `recall query blog` returned ten results, six of which
// were the word appearing inside link targets in reference sections. Every
// line here is from one of those six, and none of them is about blogging.
func TestLocatorsAreNotIndexedAsProse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		gone []string
		kept []string
	}{{
		name: "inline link destination",
		line: "- [Turso Sync](https://turso.tech/blog/introducing-databases-anywhere) - Push/pull sync",
		gone: []string{"blog", "tech", "introducing", "anywhere"},
		// The link text is what a person wrote to describe the other end.
		kept: []string{"turso", "sync", "push", "pull"},
	}, {
		name: "link text is prose and stays",
		line: "See the [Meta AI Blog](https://ai.meta.com/blog/) for launch notes",
		kept: []string{"meta", "blog", "launch", "notes"},
	}, {
		name: "link text that is itself a URL goes with the target",
		line: "compatibility note: [ollama.com/blog/claude](https://ollama.com/blog/claude)",
		gone: []string{"blog", "ollama", "claude"},
		kept: []string{"compatibility", "note"},
	}, {
		name: "bare URL inside a code span",
		line: "- announcement: `https://blog.comfy.org/p/krea-2-open-source-models`",
		gone: []string{"blog", "comfy", "krea"},
		kept: []string{"announcement"},
	}, {
		name: "scheme-less host and path",
		line: "compatibility is documented at docs.ollama.com/api/anthropic-compatibility",
		gone: []string{"api", "ollama"},
		kept: []string{"compatibility", "documented"},
	}, {
		name: "autolink",
		line: "Written up at <https://philipotoole.com/rqlite-9-0-change-data-capture/> last year",
		gone: []string{"rqlite", "capture", "philipotoole"},
		kept: []string{"written", "last", "year"},
	}, {
		name: "www host",
		line: "The directory is at www.deltadental.com/member/find-a-dentist and lists plans",
		gone: []string{"dentist", "member", "deltadental"},
		kept: []string{"directory", "lists", "plans"},
	}, {
		name: "a relative link destination is a reference too",
		line: "as described in [the adapter protocol](./adapter-protocol.md#relevance) above",
		gone: []string{"md"},
		kept: []string{"adapter", "protocol", "described", "above"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenize(withoutLocators(tc.line))
			for _, want := range tc.kept {
				if !slices.Contains(got, want) {
					t.Errorf("terms %v lost %q, which is prose", got, want)
				}
			}
			for _, unwanted := range tc.gone {
				if slices.Contains(got, unwanted) {
					t.Errorf("terms %v kept %q, which is only in a locator", got, unwanted)
				}
			}
		})
	}
}

// A corpus-relative path is how this corpus names its own documents, and naming
// a document is what makes exact-identifier matching work. It must survive.
func TestCorpusPathsSurviveTheLocatorRule(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"the whole of it is in docs/spec.md, under Index Obligations",
		"see internal/adapters/docs/index.go and eval/packs/smoke/pack.json",
		"pinned at v1.2/notes for now",
		"recall_gog/__init__.py — why these live here, the boundary, read-only",
	} {
		before, after := tokenize(line), tokenize(withoutLocators(line))
		if !slices.Equal(before, after) {
			t.Errorf("%q\n  before %v\n  after  %v", line, before, after)
		}
	}
}

// An unbalanced or truncated link must leave the rest of the document alone: a
// rule that ate to the end of the text would silently unindex whole sections.
func TestAnUnclosedLinkDestinationKeepsTheProseAroundIt(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"](",
		"nothing to see ] ( here",
	} {
		if got := withoutLocators(line); got != line {
			t.Errorf("withoutLocators(%q) = %q, want it unchanged", line, got)
		}
	}

	// The destination is unclosed, so nothing is cut as a destination. The run
	// carrying the scheme is still a locator on its own and goes — with the link
	// text glued to it, which is what a malformed link costs and is not worth
	// more machinery — and the sentence on either side survives.
	const truncated = "a truncated [link](https://example.com/path and then more prose"
	got := tokenize(withoutLocators(truncated))
	for _, want := range []string{"truncated", "then", "more", "prose"} {
		if !slices.Contains(got, want) {
			t.Errorf("terms %v lost %q after an unclosed destination", got, want)
		}
	}
	if slices.Contains(got, "example") {
		t.Errorf("terms %v kept the unclosed destination", got)
	}
}

// The two hits the ticket names, both of them Recall's own documentation using
// a user's query as a worked example, and both of them quoted. A corpus that
// quotes user queries matches those queries; a corpus that SAYS it does can be
// told apart from one that is about the subject.
func TestQuotedRunsAreTheCitationsAndNothingElse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		want []string
	}{{
		line: `separate a four-term task titled "Make a dentist appointment" from a chunk`,
		want: []string{"make", "a", "dentist", "appointment"},
	}, {
		line: "every time, because `no dentist appointment` must not read as `none, ever`",
		want: []string{"no", "dentist", "appointment", "none", "ever"},
	}, {
		line: `an apostrophe isn't a quotation and neither is a lone " mark`,
		want: nil,
	}, {
		line: `“typographic quotes” count too`,
		want: []string{"typographic", "quotes"},
	}, {
		// Hard-wrapped prose breaks a quotation across lines constantly, and the
		// sentence this rule exists for wraps between the two words that matter.
		line: "a four-term task titled \"Make a dentist\nappointment\" from a chunk",
		want: []string{"make", "a", "dentist", "appointment"},
	}, {
		line: "unclosed \"quotations do not reach the next paragraph\n\nand stop here",
		want: nil,
	}}

	for _, tc := range cases {
		got := tokenize(quotedRuns(tc.line))
		if !slices.Equal(got, tc.want) {
			t.Errorf("quotedRuns(%q)\n  got  %v\n  want %v", tc.line, got, tc.want)
		}
	}
}

// A chunk whose every occurrence of the query is a citation is not about the
// query — in a corpus that says its examples quote queries. In every other
// corpus a quoted decision IS the decision, and nothing changes.
func TestCitationsCountAgainstAboutnessOnlyWhereDeclared(t *testing.T) {
	t.Parallel()
	chunk := indexedChunk{
		Length:      100,
		Terms:       map[string]int{"dentist": 2, "relevance": 6},
		Cited:       map[string]int{"dentist": 2},
		CitedLength: 8,
	}
	coverage := queryCoverage{groups: []termGroup{{term: "dentist"}}}

	if got := coverage.aboutness(chunk); got == 0 {
		t.Error("a corpus that declares nothing discounted its own prose")
	}
	coverage.discountCitations = true
	if got := coverage.aboutness(chunk); got != 0 {
		t.Errorf("aboutness = %v over occurrences that are all citations, want 0", got)
	}

	// The chunk is still found, still scored, and still a candidate: only what
	// it claims to be about changed.
	if coverage.covered(chunk) != 1 || !coverage.admits(coverage.covered(chunk)) {
		t.Error("a declared citation stopped the chunk being a candidate at all")
	}

	// A term the document asserts in its own voice is unaffected by the
	// declaration, which is what keeps the corpus answerable about itself.
	own := queryCoverage{groups: []termGroup{{term: "relevance"}}, discountCitations: true}
	if got := own.aboutness(chunk); got == 0 {
		t.Error("a term outside every quotation was discounted")
	}
}

// Prose set against a locator with no space around it must survive: a dash is
// not part of a URL, and the run on either side of one is a word.
func TestProseGluedToALocatorByADashSurvives(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"written up here—https://example.com/blog/post—last year",
		"written up here–https://example.com/blog/post–last year",
		"written up here…https://example.com/blog/post…last year",
	} {
		got := tokenize(withoutLocators(line))
		for _, want := range []string{"written", "here", "last", "year"} {
			if !slices.Contains(got, want) {
				t.Errorf("%q\n  terms %v lost the prose word %q", line, got, want)
			}
		}
		if slices.Contains(got, "blog") {
			t.Errorf("%q\n  terms %v kept the locator", line, got)
		}
	}
}

// Findings from the independent review, each an input the first tests missed.
func TestLocatorsFoundByReview(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		gone []string
		kept []string
	}{{
		// A reference definition is the one destination the inline rule cannot
		// see, and the one most likely to be a bare relative path.
		name: "reference definition",
		line: "prose above\n\n[spec]: ./secret-target.md",
		gone: []string{"secret", "target"},
		kept: []string{"prose", "above", "spec"},
	}, {
		name: "reference definition to a URL",
		line: "[r]: https://example.com/blog/post",
		gone: []string{"blog", "example"},
		kept: []string{"r"},
	}, {
		// A line that merely starts with a bracket and has prose after a colon
		// is not a definition.
		name: "not a definition",
		line: "[note]: this sentence is ordinary prose about blogging",
		kept: []string{"note", "sentence", "ordinary", "prose", "blogging"},
	}, {
		name: "table cell wall",
		line: "|important|https://e.test/blog|other|",
		gone: []string{"blog"},
		kept: []string{"important", "other"},
	}, {
		name: "html attribute",
		line: `<a href="https://e.test/blog">important</a>`,
		gone: []string{"blog"},
		kept: []string{"important"},
	}, {
		name: "ideographic space",
		line: "important　https://e.test/blog",
		gone: []string{"blog"},
		kept: []string{"important"},
	}, {
		name: "escaped parenthesis in a destination",
		line: `[x](https://e.test/a\)secret-token) after`,
		gone: []string{"secret", "token"},
		kept: []string{"x", "after"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenize(withoutLocators(tc.line))
			for _, want := range tc.kept {
				if !slices.Contains(got, want) {
					t.Errorf("terms %v lost %q, which is prose", got, want)
				}
			}
			for _, unwanted := range tc.gone {
				if slices.Contains(got, unwanted) {
					t.Errorf("terms %v kept %q, which is only in a locator", got, unwanted)
				}
			}
		})
	}
}

// A chunk whose every word is quoted has no prose of its own, and a source that
// declared examples_quote_queries must measure it as about nothing.
// [recall.Relevance] reads a non-positive length as "this source cannot measure
// itself" and answers 1, which is right for a silent source and exactly
// inverted here.
func TestAWhollyQuotedChunkIsAboutNothing(t *testing.T) {
	t.Parallel()
	chunk := indexedChunk{
		Length:      1,
		Terms:       map[string]int{"dentist": 1},
		Cited:       map[string]int{"dentist": 1},
		CitedLength: 1,
	}
	coverage := queryCoverage{groups: []termGroup{{term: "dentist"}}, discountCitations: true}
	if got := coverage.aboutness(chunk); got != 0 {
		t.Errorf("aboutness = %v for a chunk that is nothing but a citation, want 0", got)
	}
	// Undeclared, it is ordinary text and measures what it always did.
	coverage.discountCitations = false
	if got := coverage.aboutness(chunk); got == 0 {
		t.Error("a corpus that declared nothing had its own prose discounted")
	}
}

// The variant gate asks whether the text this request may REACH spells the term
// the caller's way. Generation-wide it would see an excluded project's spelling,
// refuse to expand, and abstain over a requested project that holds the plural.
func TestTheVariantGateReadsOnlyWhatTheFilterAdmits(t *testing.T) {
	t.Parallel()
	g := &generation{
		chunks: []indexedChunk{
			{Path: "excluded/a.md", Terms: map[string]int{"goldeneye": 1}},
			{Path: "wanted/b.md", Terms: map[string]int{"goldeneyes": 1}},
		},
		postings: map[string][]posting{
			"goldeneye":  {{chunk: 0, tf: 1}},
			"goldeneyes": {{chunk: 1, tf: 1}},
		},
	}
	wanted := func(c indexedChunk) bool { return strings.HasPrefix(c.Path, "wanted/") }

	groups := groupTerms(g, []string{"goldeneye"}, wanted)
	if len(groups) != 1 || !slices.Contains(groups[0].variants, "goldeneyes") {
		t.Fatalf("groups = %+v, want the plural resolved inside the filter", groups)
	}
	// Unfiltered, the corpus holds the caller's own spelling and is not widened.
	if groups := groupTerms(g, []string{"goldeneye"}, nil); len(groups[0].variants) != 0 {
		t.Errorf("an unfiltered corpus holding the term was widened to %v", groups[0].variants)
	}
}
