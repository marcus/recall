package docs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func termSet(list ...string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, t := range list {
		out[t] = true
	}
	return out
}

// The whole point of the change: the window is cut around the line carrying the
// term, not from the head of the chunk.
func TestExcerptWindowIsCutAroundTheMatchedLine(t *testing.T) {
	t.Parallel()
	const head = "Nothing in this system depends on OpenClaw running."
	const match = "`com.marcus.perch` keeps planner, blog, and dispatch databases open there."
	body := []string{head, ""}
	for range 20 {
		body = append(body, strings.Repeat("filler ", 12))
	}
	body = append(body, match, "", "Freezing it is deferred.")

	got, ok := excerptWindow(body, termSet("blog"))
	if !ok {
		t.Fatal("no window selected for a term that is in the body")
	}
	if !strings.Contains(got, match) {
		t.Errorf("window %q does not contain the line that matched", got)
	}
	if strings.Contains(got, head) {
		t.Errorf("window %q is still the head of the chunk", got)
	}
	// Enough of what precedes the match to read as a sentence, and no more.
	lead, _, _ := strings.Cut(got, match)
	if n := utf8.RuneCountInString(lead); n > leadRunes {
		t.Errorf("window spends %d runes on lead-in, budget is %d", n, leadRunes)
	}
	// Blank lines are dropped, not crossed as content.
	if !strings.Contains(got, "there. Freezing it is deferred.") {
		t.Errorf("window %q did not join across the paragraph break", got)
	}
}

// Coverage decides between windows, so a query naming two things prefers the
// place both are said over an earlier place only one is.
func TestExcerptWindowPrefersTheWindowCoveringMostTerms(t *testing.T) {
	t.Parallel()
	// Far enough apart that no window reaches from the first mention to the
	// place both terms are said.
	body := []string{"The planner runs first."}
	for range 20 {
		body = append(body, strings.Repeat("filler ", 12))
	}
	body = append(body, "The planner and the dispatch database open together.")

	got, ok := excerptWindow(body, termSet("planner", "dispatch"))
	if !ok {
		t.Fatal("no window selected")
	}
	if !strings.Contains(got, "The planner and the dispatch database open together.") {
		t.Errorf("window %q, want the one covering both terms", got)
	}
	if strings.Contains(got, "The planner runs first.") {
		t.Errorf("window %q is the earlier window covering one term", got)
	}
}

// Earliest wins ties, which is what makes selection reproducible; an eval run
// compares excerpts across runs and cannot do that against a coin flip.
func TestExcerptWindowIsDeterministic(t *testing.T) {
	t.Parallel()
	body := []string{
		"The planner opens the database.",
		"The planner closes the database.",
		"The planner reopens the database.",
	}
	terms := termSet("planner", "database", "opens")

	first, ok := excerptWindow(body, terms)
	if !ok {
		t.Fatal("no window selected")
	}
	if first != "The planner opens the database. The planner closes the database. "+
		"The planner reopens the database." {
		t.Fatalf("window = %q, want the earliest window of maximal coverage", first)
	}
	for i := range 50 {
		got, _ := excerptWindow(body, terms)
		if got != first {
			t.Fatalf("run %d selected %q, want %q", i, got, first)
		}
	}
}

// The bound is the one thing selection may not buy its way out of.
func TestExcerptWindowStaysWithinTheBound(t *testing.T) {
	t.Parallel()
	var body []string
	for range 40 {
		body = append(body, strings.Repeat("filler ", 12))
	}
	body = append(body, "the dentist appointment is on Tuesday")
	for range 40 {
		body = append(body, strings.Repeat("filler ", 12))
	}

	got, ok := excerptWindow(body, termSet("dentist"))
	if !ok {
		t.Fatal("no window selected")
	}
	if n := utf8.RuneCountInString(got); n > excerptRunes {
		t.Errorf("window is %d runes, bound is %d", n, excerptRunes)
	}
	if !strings.Contains(got, "dentist") {
		t.Errorf("window %q dropped the term it was selected for", got)
	}
}

// A single line longer than the bound is the case where a front-truncating cut
// would delete the very thing the excerpt exists to show.
func TestExcerptWindowCutsAroundAMatchInAnOverlongLine(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("lead ", 200) + "dentist " + strings.Repeat("tail ", 200)

	got, ok := excerptWindow([]string{line}, termSet("dentist"))
	if !ok {
		t.Fatal("no window selected")
	}
	if n := utf8.RuneCountInString(got); n > excerptRunes {
		t.Errorf("window is %d runes, bound is %d", n, excerptRunes)
	}
	if !strings.Contains(got, "dentist") {
		t.Errorf("window %q dropped the term it was cut around", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("window %q does not mark both ends it cut", got)
	}
}

func TestExcerptWindowReportsNoMatch(t *testing.T) {
	t.Parallel()
	body := []string{"The planner opens the database."}

	if got, ok := excerptWindow(body, termSet("dentist")); ok {
		t.Errorf("window %q selected for a term that is not in the body", got)
	}
	if got, ok := excerptWindow(body, nil); ok {
		t.Errorf("window %q selected for an empty query", got)
	}
	if got, ok := excerptWindow(nil, termSet("planner")); ok {
		t.Errorf("window %q selected from an empty body", got)
	}
}

// The excerpt is anchored on the same terms the diagnostics call the query, so
// scaffolding cannot anchor a window on a word nobody asked about.
func TestExcerptTermsExcludeFunctionWords(t *testing.T) {
	t.Parallel()
	got := excerptTerms(analyzeQuery("what is the wifi password"))

	for _, want := range []string{"wifi", "password"} {
		if !got[want] {
			t.Errorf("excerpt terms %v are missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"what", "is", "the"} {
		if got[unwanted] {
			t.Errorf("excerpt terms %v retained the function word %q", got, unwanted)
		}
	}
}

func TestCutAroundKeepsTheBoundAtEveryPosition(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("abcde", 200)

	for _, at := range []int{0, 1, 99, 500, 998, 999} {
		got := cutAround(text, at, excerptRunes)
		if n := utf8.RuneCountInString(got); n > excerptRunes {
			t.Errorf("cut at %d is %d runes, bound is %d", at, n, excerptRunes)
		}
	}
	if got := cutAround("short", 0, excerptRunes); got != "short" {
		t.Errorf("cut of text within the bound = %q, want it unchanged", got)
	}
}

// The two hashes exist for opposite reasons, and this is the case that shows
// why the excerpt gate cannot be the fingerprint: reflowing a paragraph is the
// same tokens, so the fingerprint holds — which is what corroboration needs —
// while every offset the window is cut at has moved.
func TestBodyDigestSeesWhatTheFingerprintIsBuiltToIgnore(t *testing.T) {
	t.Parallel()
	indexed := parsedChunk{
		Heading: "## Indexing",
		Body: []string{
			"The builder writes a whole generation and publishes it with a single rename, so",
			"an interrupted build costs freshness and nothing else.",
		},
	}
	reflowed := parsedChunk{
		Heading: "## Indexing",
		Body: []string{
			"The builder writes a whole generation and publishes it with a single rename",
			"— so an interrupted build costs freshness and nothing else!",
		},
	}

	if indexed.fingerprint() != reflowed.fingerprint() {
		t.Fatal("the reflow changed the tokens; it no longer isolates the two hashes")
	}
	if bodyDigest(indexed.Body) == bodyDigest(reflowed.Body) {
		t.Error("body digest ignored a byte change, so a moved window would pass the gate")
	}

	// And the window really does move, which is the whole reason the gate has
	// to be the digest: the same query over the same generation would otherwise
	// return two different excerpts under one watermark.
	terms := termSet("rename")
	from, _ := excerptWindow(indexed.Body, terms)
	to, _ := excerptWindow(reflowed.Body, terms)
	if from == to {
		t.Errorf("both bodies selected %q; the reflow no longer moves the window", from)
	}
}

// The lead-in is a share of the bound, so a window that spends it still fits.
func TestExcerptWindowWithLeadInStaysWithinTheBound(t *testing.T) {
	t.Parallel()
	var body []string
	for range 30 {
		body = append(body, strings.Repeat("filler ", 12))
	}
	body = append(body, "the dentist appointment is on Tuesday")
	body = append(body, strings.Repeat("trailer ", 12))

	got, ok := excerptWindow(body, termSet("dentist"))
	if !ok {
		t.Fatal("no window selected")
	}
	if n := utf8.RuneCountInString(got); n > excerptRunes {
		t.Errorf("window is %d runes, bound is %d", n, excerptRunes)
	}
	if !strings.Contains(got, "dentist") {
		t.Errorf("window %q dropped the term it was selected for", got)
	}
	if !strings.HasPrefix(got, "filler") {
		t.Errorf("window %q spent none of its lead-in budget", got)
	}
}
