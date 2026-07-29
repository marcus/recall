package qmd

import (
	"strings"
	"testing"
)

// The snippet header is what a locator's line range is parsed out of. Without it
// expansion has nothing to point at, which is the one thing the spike had to
// prove before any of this was worth writing.
func TestSnippetHeaderYieldsTheSpan(t *testing.T) {
	hit := searchHit{
		DocID:   "#43f92c",
		File:    "qmd://fixture/notes/tooth-care.md",
		Line:    6,
		Title:   "Tooth care appointment",
		Snippet: "@@ -5,4 @@ (4 before, 15 after)\nFind a dental hygienist\nplan and patients.\n\n## Recommendation",
	}
	got, title, body, err := parseHit(hit, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "notes/tooth-care.md" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Start != 5 || got.End != 8 {
		t.Errorf("span = L%d-L%d, want L5-L8", got.Start, got.End)
	}
	if !got.Stated {
		t.Error("a header-stated span must not be reported as inferred")
	}
	if got.Before != 4 || got.After != 15 {
		t.Errorf("context = %d/%d", got.Before, got.After)
	}
	if got.Local() != "notes/tooth-care.md#L5-L8" {
		t.Errorf("local = %q", got.Local())
	}
	if title != "Tooth care appointment" {
		t.Errorf("title = %q", title)
	}
	if strings.HasPrefix(body, "@@") {
		t.Error("the header leaked into the excerpt")
	}
}

// Without a header the anchor line is the only statement about position left, and
// the span is as many lines as the snippet carries — a claim about the snippet
// rather than about the document, so it is marked.
func TestSpanWithoutAHeaderIsMarkedInferred(t *testing.T) {
	hit := searchHit{
		DocID:   "#43f92c",
		File:    "qmd://fixture/notes/tooth-care.md",
		Line:    12,
		Snippet: "Find a dental hygienist\nplan and patients.",
	}
	got, title, _, err := parseHit(hit, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stated {
		t.Error("an inferred span claimed to be stated")
	}
	if got.Start != 12 || got.End != 13 {
		t.Errorf("span = L%d-L%d, want L12-L13", got.Start, got.End)
	}
	if title != "tooth-care.md" {
		t.Errorf("a titleless result must fall back to the file name, got %q", title)
	}
}

func TestParseHitRefusesUnusableResults(t *testing.T) {
	cases := map[string]searchHit{
		"no scheme":       {File: "/notes/a.md"},
		"no document":     {File: "qmd://fixture/"},
		"no collection":   {File: "qmd:///notes/a.md"},
		"escaping path":   {File: "qmd://fixture/../../etc/passwd"},
		"unnormal path":   {File: "qmd://fixture/notes/./a.md"},
		"absolute inside": {File: "qmd://fixture//etc/passwd"},
	}
	for name, hit := range cases {
		if _, _, _, err := parseHit(hit, "fixture"); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// A foreign collection is a distinct fact: it is dropped and counted rather
	// than treated as unreadable, because the search boundary is what it puts in
	// doubt.
	if _, _, _, err := parseHit(searchHit{File: "qmd://elsewhere/a.md"}, "fixture"); err == nil ||
		!strings.Contains(err.Error(), "another collection") {
		t.Errorf("a foreign collection gave %v", err)
	}
}

// Relevance is coverage times concentration over the text this source has, and
// it is the one number compared across sources. qmd's own score must not reach
// it.
func TestSpanRelevanceMeasuresTheReturnedText(t *testing.T) {
	terms := queryTerms("dental hygienist plan")
	on := spanRelevance(terms, "Tooth care", "Find a dental hygienist who takes the Sample Dental plan.")
	off := spanRelevance(terms, "Sourdough starter", "Equal parts flour and water by weight.")
	switch {
	case on <= 0:
		t.Fatalf("a covered span measured %v", on)
	case off != 0:
		t.Fatalf("an uncovered span measured %v, want 0", off)
	case on > 1:
		t.Fatalf("relevance %v is out of range", on)
	}
	// A browse has nothing to be about, and the shared definition answers 1 for
	// it rather than 0 — a zero would demote every record against a query that
	// asked nothing.
	if got := spanRelevance(nil, "Tooth care", "anything"); got != 1 {
		t.Fatalf("an empty query measured %v, want 1", got)
	}
	// Unmeasurable length is the same case: guessing low would demote a source
	// that failed to report a length rather than one that matched badly.
	if got := spanRelevance(terms, "", ""); got != 1 {
		t.Fatalf("an empty record measured %v, want 1", got)
	}
}

// The retention rule has to match the built-in lexical document adapter's, or the
// two sources report coverage fractions with different denominators and the
// numbers stop being comparable.
func TestQueryTermsDropFunctionWordsOnly(t *testing.T) {
	got := strings.Join(queryTerms("What is the dentist appointment"), " ")
	if got != "dentist appointment" {
		t.Fatalf("terms = %q", got)
	}
	// A query made entirely of scaffolding falls back to its raw terms rather
	// than becoming a query with nothing to be about.
	if got := strings.Join(queryTerms("what is the"), " "); got != "what is the" {
		t.Fatalf("terms = %q", got)
	}
	// Distinct terms, in first-seen order: coverage is a fraction over distinct
	// terms and a repeated word must not inflate the denominator.
	if got := strings.Join(queryTerms("dentist dentist appointment"), " "); got != "dentist appointment" {
		t.Fatalf("terms = %q", got)
	}
}

// The per-result signals are bounded and sanitized. The expanded query strings
// are model output about source text, and the core's sanitizer walks only
// top-level fields — this list is nested.
func TestComponentSignalsAreBoundedAndSanitized(t *testing.T) {
	// Escapes, not literals: a source file spelling out a bidi override carries
	// it into the next tool that reads the file.
	hostile := "line one\nline two \u202ereversed\u202c"
	contributions := make([]contribution, 0, maxComponents*2)
	for i := 0; i < maxComponents*2; i++ {
		contributions = append(contributions, contribution{
			Source: "vec", QueryType: "hyde", Query: hostile + strings.Repeat("x", 500), Rank: i + 1,
		})
	}
	got := boundedComponents(contributions)
	if len(got) != maxComponents {
		t.Fatalf("components = %d, want at most %d", len(got), maxComponents)
	}
	query, _ := got[0]["query"].(string)
	switch {
	case strings.ContainsAny(query, "\n\r"):
		t.Errorf("a nested string carries a line break: %q", query)
	case strings.Contains(query, "\u202e"), strings.Contains(query, "\u202c"):
		t.Errorf("a nested string carries a bidi override: %q", query)
	case len(query) > maxComponentQuery:
		t.Errorf("a nested string is %d bytes", len(query))
	}
}

// The original query is not an expansion, and one expansion reported by five
// results is one expansion.
func TestExpandedQueriesDeduplicateAndExcludeTheOriginal(t *testing.T) {
	trace := &explain{}
	trace.RRF.Contributions = []contribution{
		{Source: "vec", QueryType: "original", Query: "who can clean my teeth"},
		{Source: "vec", QueryType: "hyde", Query: "a hypothetical answer"},
		{Source: "fts", QueryType: "lex", Query: "dental hygiene"},
	}
	hits := []searchHit{{Explain: trace}, {Explain: trace}, {}}
	got := expandedQueries(hits, "who can clean my teeth")
	if len(got) != 2 {
		t.Fatalf("expanded = %v", got)
	}
	for _, entry := range got {
		if entry["query"] == "who can clean my teeth" {
			t.Error("the original query was reported as an expansion")
		}
	}
}
