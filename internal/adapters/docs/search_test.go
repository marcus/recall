package docs_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// TestChunksOfOneDocumentShareRecordID is the load-bearing one.
//
// Corroboration collapses lineage groups that share source_uid plus
// source_record_id. If each chunk carried its own record id, two sections of
// one file would count as two independent units and this source would
// corroborate itself — a document would outrank the same claim confirmed by a
// second source. See docs/spec.md#4-clustering-and-corroboration.
func TestChunksOfOneDocumentShareRecordID(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	// "recall" appears in several sections of architecture.md.
	resp := search(t, a, "recall ranking indexing corroboration generation")

	const doc = "projects/recall/architecture.md"
	locators := map[string]bool{}
	for _, c := range resp.Candidates {
		if c.SourceRecordID != doc {
			continue
		}
		if got := metaString(t, c, "path"); got != doc {
			t.Errorf("candidate %s: metadata path %q, want %q", c.CandidateID, got, doc)
		}
		locators[c.Locator.Local] = true
	}
	if len(locators) < 2 {
		t.Fatalf("want at least two chunks of %s in the results, got %d", doc, len(locators))
	}

	// Distinct locators, one record identity: the chunks are separately
	// expandable and jointly one piece of evidence.
	for _, c := range resp.Candidates {
		if strings.HasPrefix(c.Locator.Local, doc+"#") && c.SourceRecordID != doc {
			t.Errorf("chunk %s reports record id %q, want the document %q",
				c.Locator.Local, c.SourceRecordID, doc)
		}
	}
}

// TestChunkingFollowsHeadings checks the two chunk boundaries that are easy to
// get wrong: a fenced code block that contains a heading-shaped line, and the
// line range a locator has to name.
func TestChunkingFollowsHeadings(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doc = "projects/recall/architecture.md"
	indexing := lineOf(t, root, doc, "## Indexing")
	next := lineOf(t, root, doc, "## Open Questions")

	// The fenced "# this heading-looking line" sits inside the Indexing
	// section. If the chunker treated it as a heading, this term would land in
	// a chunk of its own starting at that line.
	resp := search(t, a, "heading-looking line inside a fence")
	c := firstFrom(t, resp, doc)

	if got := metaInt(t, c, "start_line"); got != indexing {
		t.Errorf("chunk starts at line %d, want the Indexing heading at %d: a fenced line opened a section",
			got, indexing)
	}
	if got := metaInt(t, c, "end_line"); got >= next {
		t.Errorf("chunk ends at line %d, want before the next heading at %d", got, next)
	}
	if got := c.Locator.Local; got != fmt.Sprintf("%s#L%d-L%d", doc, metaInt(t, c, "start_line"), metaInt(t, c, "end_line")) {
		t.Errorf("locator %q does not match its own line range metadata", got)
	}

	path, ok := c.Metadata["heading_path"].([]string)
	if !ok {
		t.Fatalf("heading_path is %T, want []string kept as typed metadata", c.Metadata["heading_path"])
	}
	want := []string{"Recall Architecture", "Indexing"}
	if strings.Join(path, ">") != strings.Join(want, ">") {
		t.Errorf("heading_path = %v, want %v", path, want)
	}
	if c.Title != "Recall Architecture > Indexing" {
		t.Errorf("title = %q, want the heading path without the repeated document title", c.Title)
	}
}

// TestChunkingIsDeterministic rebuilds an unchanged corpus and demands the same
// chunks, in the same order, under the same locators. Evaluation replays
// locators recorded in a judgment pack; a chunker that drifts between builds
// would invalidate every judgment on every rebuild.
func TestChunkingIsDeterministic(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	const query = "recall ranking indexing corroboration clara signals memory adr deletion priors corpus"
	before := search(t, a, query)
	if len(before.Candidates) < 8 {
		t.Fatalf("query matched %d chunks, too few to prove anything", len(before.Candidates))
	}

	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after := search(t, a, query)

	if len(before.Candidates) != len(after.Candidates) {
		t.Fatalf("chunk count changed across rebuilds: %d then %d",
			len(before.Candidates), len(after.Candidates))
	}
	for i := range before.Candidates {
		x, y := before.Candidates[i], after.Candidates[i]
		switch {
		case x.Locator.Local != y.Locator.Local:
			t.Errorf("rank %d: locator %q then %q", i+1, x.Locator.Local, y.Locator.Local)
		case x.CandidateID != y.CandidateID:
			t.Errorf("rank %d: candidate id %q then %q", i+1, x.CandidateID, y.CandidateID)
		case x.ContentFingerprint != y.ContentFingerprint:
			t.Errorf("rank %d: fingerprint %q then %q", i+1, x.ContentFingerprint, y.ContentFingerprint)
		case x.Excerpt != y.Excerpt:
			t.Errorf("rank %d: excerpt changed across rebuilds", i+1)
		}
	}
}

// TestRankingIsStableAcrossRebuilds is the ordering half of the same promise.
// Scores come from term statistics over the whole corpus, so a rebuild that
// visited files in a different order, or broke a tie differently, would reorder
// results without a single document changing.
func TestRankingIsStableAcrossRebuilds(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	queries := []string{
		"how are indexes published",
		"corroboration",
		"signals projection upstream",
		"decisions about deletion",
		// A question, so the coordination factor is exercised too: it scales
		// every score, and a rebuild has to reproduce the scaled number and
		// the order it produces, not just the BM25 sum underneath it.
		"what does the recall architecture say about deletion",
	}

	first := make(map[string][]string, len(queries))
	for _, q := range queries {
		first[q] = ranking(search(t, a, q))
	}

	for round := range 2 {
		if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
			t.Fatalf("rebuild %d: %v", round, err)
		}
		for _, q := range queries {
			got := ranking(search(t, a, q))
			if strings.Join(got, "\n") != strings.Join(first[q], "\n") {
				t.Errorf("query %q reordered after rebuild %d:\nfirst %v\nnow   %v",
					q, round+1, first[q], got)
			}
		}
	}
}

// ranking renders the order and the scores a rebuild must reproduce exactly.
func ranking(resp recall.SearchResponse) []string {
	out := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		score := 0.0
		if c.LocalScore != nil {
			score = *c.LocalScore
		}
		out = append(out, fmt.Sprintf("%d %s %.12f", c.LocalRank, c.Locator.Local, score))
	}
	return out
}

// TestLocalRankIsDenseAndOrdered checks the one mandatory relevance signal.
// Fusion consumes local_rank and nothing else, so a gap or a repeat here would
// silently distort cross-source ordering.
func TestLocalRankIsDenseAndOrdered(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	resp := search(t, a, "recall indexing ranking")
	for i, c := range resp.Candidates {
		if c.LocalRank != i+1 {
			t.Fatalf("candidate %d has local_rank %d, want %d", i, c.LocalRank, i+1)
		}
		if !c.HasSignal(recall.MatchLexical) {
			t.Errorf("%s carries no lexical signal", c.Locator.Local)
		}
	}
}

func TestNaturalQuestionAndKeywordFormShareRetrieval(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	// This pair is the adapter-level version of dev-pack cases
	// abstain-wifi-016 and abstain-sentence-018. Neither content term exists,
	// so grammatical scaffolding must not manufacture twenty weak candidates.
	for _, query := range []string{"wifi password", "what is the wifi password"} {
		resp := search(t, a, query)
		if len(resp.Candidates) != 0 {
			t.Errorf("%q returned %d candidates, want an empty successful search", query, len(resp.Candidates))
		}
		if resp.Outcome != recall.SearchSuccess {
			t.Errorf("%q outcome = %q, want successful coverage of an empty result", query, resp.Outcome)
		}
	}

	question := search(t, a, "what is the wifi password")
	if got := question.Diagnostics["query_analyzer"]; got != "alnum-fold+english-function-words-v2" {
		t.Errorf("query analyzer diagnostic = %v", got)
	}
	if got := question.Diagnostics["query_terms_removed"]; got != 3 {
		t.Errorf("removed terms diagnostic = %v, want 3", got)
	}
}

func TestEveryCandidateCarriesContentEvidence(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		"content.md":     "# Network\nThe wifi password is printed on the router.\n",
		"scaffolding.md": "# What is the\nWhat is the what is the.\n",
	}), nil)

	resp := search(t, a, "what is the wifi password")
	if len(resp.Candidates) == 0 {
		t.Fatal("fixture content term did not produce a candidate")
	}
	for _, candidate := range resp.Candidates {
		path := metaString(t, candidate, "path")
		if path == "scaffolding.md" {
			t.Fatalf("scaffolding-only chunk entered candidates: %+v", candidate)
		}
		if path != "content.md" {
			t.Errorf("unexpected candidate path %q", path)
		}
	}
	if got := resp.Diagnostics["query_scoring"]; got != "full_query_over_content_candidates" {
		t.Errorf("query scoring diagnostic = %v", got)
	}
}

// TestQuestionAbstainsWhenOneOfItsTermsIsAbsent is the price of measuring
// coverage against what was asked instead of against what the corpus turned out
// to hold. A question about a wifi password, in a corpus that documents the
// network and no password, is answered with nothing rather than with everything
// about the network. On the home profile the same rule takes "what is the zxqv
// project" from 64 document results to none.
//
// It is a deliberate trade and it has a recourse: the term on its own, or the
// document's name, both of which say something a question does not, which is
// that the caller meant exactly this much.
func TestQuestionAbstainsWhenOneOfItsTermsIsAbsent(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		"network.md": "# Network\nThe wifi network is documented here, on the shelf.\n",
	}), nil)

	resp := search(t, a, "what is the wifi password")
	if len(resp.Candidates) != 0 {
		t.Errorf("question returned %v; the corpus has no password and coverage says so",
			paths(t, resp))
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %q, want a successful search that found nothing", resp.Outcome)
	}

	// Same question, both terms present: it is the absent term that abstained,
	// not the shape of the query.
	if got := paths(t, search(t, a, "what is the wifi network")); len(got) != 1 {
		t.Errorf("question over two present terms returned %v, want the document", got)
	}
	// The recourse.
	if got := paths(t, search(t, a, "wifi")); len(got) != 1 {
		t.Errorf("the term on its own returned %v, want the document", got)
	}
}

// TestQuestionNeedsMoreThanOneOfItsTerms is the defect td-92c7b7 was written
// for: asking a question instead of typing a keyword made the answer four times
// larger, because one common word of the question admitted every chunk that
// held it and BM25 has no coordination factor to sort them back down.
//
// The corpus is the shape that produced it. "sidecar" names one document;
// "project" and "for" are the words the rest of the corpus is made of.
func TestQuestionNeedsMoreThanOneOfItsTerms(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		// Three of the query's terms, two, one and one.
		"sidecar.md": "# Sidecar\nSidecar is the project for watching AI coding work as it happens.\n",
		"index.md":   "# Index\nEvery project in this directory records what the project is for.\n",
		"tools.md":   "# Tools\nThe backup project runs nightly. Every project is listed here.\n",
		"people.md":  "# People\nMarcus writes small tools for the joy of the craft.\n",
	}), nil)

	// The weak term still reaches its own documents: what changes is what a
	// question does with it, not whether the corpus holds it.
	if got := paths(t, search(t, a, "project")); len(got) != 3 {
		t.Fatalf("the keyword %q returned %v, want the 3 documents that use it", "project", got)
	}

	question := search(t, a, "what is the sidecar project for")
	if got := paths(t, question); strings.Join(got, ",") != "sidecar.md,index.md" {
		t.Errorf("the question returned %v, want the document it is about and then the one that "+
			"covers two of its three terms; one term is no longer enough to answer a question", got)
	}
	if got := question.Diagnostics["query_terms_required"]; got != 2 {
		t.Errorf("coverage diagnostic = %v, want the floor a caller can read a thin result against", got)
	}

	// Bare keywords are not a question. The caller chose every one of those
	// words and nothing here knows which were meant as alternatives, so the
	// floor stays off and coverage only orders them.
	keywords := search(t, a, "sidecar project")
	if got := paths(t, keywords); len(got) != 3 || got[0] != "sidecar.md" {
		t.Errorf("keyword form returned %v, want the disjunction it has always been with the "+
			"document carrying both terms first", got)
	}
}

// TestPartialMatchesSortBelowCompleteOnes is the ordering half, and the fixture
// is built so that BM25 alone gets it wrong: watcher.md is short and repeats
// "telemetry" four times, so on term frequency and length normalization it
// outscores a document that answers the whole query once. Delete the
// coordination factor and this test fails, which is the only way a test of a
// factor means anything.
func TestPartialMatchesSortBelowCompleteOnes(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		"watcher.md": "# Watcher\nTelemetry, telemetry, telemetry. The watcher reads telemetry.\n",
		"protocol.md": "# Protocol\nThe protocol carries telemetry from the adapter to the core, " +
			"where the fusion stage reads it and hands it on. Nothing in this file depends on " +
			"the transport, and the same messages travel unchanged over a socket later. " +
			"Adapters publish generations, the core resolves locators against them, and every " +
			"record it rejects is reported rather than dropped in silence.\n",
		"stage.md": "# Stage\nThe fusion stage runs after every adapter has answered, or has " +
			"failed to.\n",
	}), nil)

	// On the term they share, the short document that repeats it wins outright.
	one := search(t, a, "telemetry")
	if got := paths(t, one); len(got) != 2 || got[0] != "watcher.md" {
		t.Fatalf("single-term order is %v, want watcher.md first: the fixture no longer sets up the flip", got)
	}

	// Add a term only one of them carries and the order reverses. Nothing about
	// watcher.md changed except the share of the query it answers, and its raw
	// BM25 is still the larger of the two.
	both := search(t, a, "telemetry stage")
	if got := paths(t, both); len(got) != 3 || got[0] != "protocol.md" {
		t.Errorf("order is %v, want protocol.md first: a partial match outranked a complete one", got)
	}
}

// TestSingleTermQueryIsUnaffectedByCoverage. One term is either present or it
// is not, so coverage is 1 or the chunk was never scored — the guard is that
// no arithmetic on the way there can make a one-word query return less than the
// documents that hold the word. `bonnie` and `dentist` are that query.
func TestSingleTermQueryIsUnaffectedByCoverage(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		"owner.md":   "# Owner\nBonnie is the one who books the dentist.\n",
		"routine.md": "# Routine\nThe dentist appointment is a standing one, twice a year.\n",
		"tools.md":   "# Tools\nNothing here has anything to do with either of them.\n",
	}), nil)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{query: "bonnie", want: []string{"owner.md"}},
		{query: "dentist", want: []string{"owner.md", "routine.md"}},
		// A question around one content term is still one content term.
		{query: "who is bonnie", want: []string{"owner.md"}},
	} {
		got := paths(t, search(t, a, tc.query))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("query %q returned %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestNamedDocumentSurvivesTheCoverageFloor. Naming a file is a request for
// that file, and it outranks everything the text of the query would have found.
// A floor that applied to it would answer "docs/spec.md" with nothing whenever
// the rest of the sentence was about something the file does not say.
func TestNamedDocumentSurvivesTheCoverageFloor(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, writtenCorpus(t, map[string]string{
		"specs/protocol.md": "# Protocol\nAdapters speak newline-delimited JSON-RPC over stdio.\n",
		"index.md":          "# Index\nEvery project in this directory records what the project is for.\n",
		"tools.md":          "# Tools\nThe backup project runs nightly. Notes for each project live here.\n",
	}), nil)

	resp := search(t, a, "what does specs/protocol.md say about the sidecar project")
	c := firstFrom(t, resp, "specs/protocol.md")
	if !c.Exact() {
		t.Errorf("%s did not carry exact_identifier for the name the query used", c.Locator.Local)
	}
	if c.LocalRank != 1 {
		t.Errorf("the named document ranked %d, want first", c.LocalRank)
	}
}

// paths renders a response as the documents it answered with, in order.
func paths(t *testing.T, resp recall.SearchResponse) []string {
	t.Helper()
	out := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, metaString(t, c, "path"))
	}
	return out
}

// TestExactIdentifierOnlyAtTokenBoundaries is the false-positive test the spec
// asks for. exact_identifier partitions the entire result set above everything
// without it, so a signal emitted for prose that merely contains a file's words
// would promote a weak match over a strong one, everywhere, forever.
func TestExactIdentifierOnlyAtTokenBoundaries(t *testing.T) {
	t.Parallel()
	aliases := map[string]any{
		"aliases": map[string]any{
			"projects/recall/decisions.md": []any{"ADR log"},
		},
	}
	a, _ := newAdapter(t, cleanCorpus(t), aliases)

	const decisions = "projects/recall/decisions.md"
	const architecture = "projects/recall/architecture.md"

	cases := []struct {
		name  string
		query string
		doc   string
		exact bool
		alias bool
	}{
		{
			name:  "full path",
			query: "what does projects/recall/decisions.md say about deletion",
			doc:   decisions,
			exact: true,
		},
		{
			name:  "basename with extension",
			query: "open decisions.md",
			doc:   decisions,
			exact: true,
		},
		{
			name:  "path without extension",
			query: "projects/recall/decisions please",
			doc:   decisions,
			exact: true,
		},
		{
			// A bare stem is a word. Promoting "decisions" as an identifier
			// would make every discussion of decisions an exact hit on this
			// one file.
			name:  "bare stem is not an identifier",
			query: "decisions about deletion",
			doc:   decisions,
			exact: false,
		},
		{
			// The prose that the file name is made of must stay lexical: the
			// document is about architecture, it is not NAMED by the sentence.
			name:  "prose containing the file words",
			query: "the recall architecture of indexing",
			doc:   architecture,
			exact: false,
		},
		{
			// Same characters, wrong token: a substring test would match here.
			name:  "different extension",
			query: "decisions.markdown",
			doc:   decisions,
			exact: false,
		},
		{
			name:  "declared alias",
			query: "where is the adr log",
			doc:   decisions,
			exact: true,
			alias: true,
		},
		{
			// "adrs" is a different token from "adr"; only a substring match
			// would find the alias inside it.
			name:  "alias inside a longer token",
			query: "adrs log",
			doc:   decisions,
			exact: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := search(t, a, tc.query)
			var got, gotAlias, present bool
			for _, c := range resp.Candidates {
				if c.SourceRecordID != tc.doc {
					continue
				}
				present = true
				got = got || c.Exact()
				gotAlias = gotAlias || c.HasSignal(recall.MatchAlias)
			}
			// A negative case only means something if the document was in the
			// results at all: otherwise "no exact signal" would pass for a
			// query that simply found nothing.
			if !present {
				t.Fatalf("query %q returned nothing from %s", tc.query, tc.doc)
			}
			if got != tc.exact {
				t.Errorf("query %q: exact_identifier on %s = %v, want %v", tc.query, tc.doc, got, tc.exact)
			}
			if gotAlias != tc.alias {
				t.Errorf("query %q: alias signal on %s = %v, want %v", tc.query, tc.doc, gotAlias, tc.alias)
			}
		})
	}
}

// TestExactIdentifierSurfacesADocumentTheTextWouldNotMatch: naming a file is a
// request for that file. A source that answered nothing because the file name
// is absent from its own prose would be useless for the most precise query
// anyone can type.
func TestExactIdentifierSurfacesADocumentTheTextWouldNotMatch(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	resp := search(t, a, "status.md")
	c := firstFrom(t, resp, "projects/recall/status.md")
	if !c.Exact() {
		t.Errorf("%s did not carry exact_identifier for its own file name", c.Locator.Local)
	}
	if c.LocalRank != 1 {
		t.Errorf("named document ranked %d, want first in this source's own order", c.LocalRank)
	}
}

// TestProjectFilterUsesCorpusStructure keeps the directory layout as routing
// metadata instead of flattening it into text, per the document source class.
func TestProjectFilterUsesCorpusStructure(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	resp := search(t, a, "project notes and ranking", func(req *recall.SearchRequest) {
		req.Filters.Project = "projects"
	})
	if len(resp.Candidates) == 0 {
		t.Fatal("project filter excluded everything")
	}
	for _, c := range resp.Candidates {
		if got := metaString(t, c, "project"); got != "projects" {
			t.Errorf("%s has project %q under a filter for %q", c.Locator.Local, got, "projects")
		}
	}

	none := search(t, a, "project notes and ranking", func(req *recall.SearchRequest) {
		req.Filters.Project = "nothing-by-that-name"
	})
	if len(none.Candidates) != 0 {
		t.Errorf("filter for an unknown project returned %d candidates", len(none.Candidates))
	}
	if none.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %q; a filter that matched nothing is still a successful search", none.Outcome)
	}
}

// TestSearchReportsTheRevisionItSearched. Every result reports which revision
// answered it; without it a stale generation and a current one are
// indistinguishable downstream.
func TestSearchReportsTheRevisionItSearched(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	resp := search(t, a, "corroboration")
	if resp.SourceWatermark == "" {
		t.Fatal("search reported no source watermark")
	}
	for _, c := range resp.Candidates {
		if c.SourceRevision != resp.SourceWatermark {
			t.Errorf("%s reports revision %q, response says %q",
				c.Locator.Local, c.SourceRevision, resp.SourceWatermark)
		}
		if c.ConfirmedAt == nil {
			t.Errorf("%s has no confirmed_at after a complete boundary", c.Locator.Local)
		}
	}
	if diag, ok := resp.Diagnostics["generation"].(string); !ok || diag == "" {
		t.Error("diagnostics do not name the generation that answered")
	}
}

// TestExcerptShowsTheSpanThatMatched is the one an agent's first session paid
// for: the indexed excerpt is the head of the chunk, so a term further down
// produced a hit whose displayed text carried nothing of the query, and
// establishing that the hit was real took reading the file by hand.
func TestExcerptShowsTheSpanThatMatched(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	// "rename" is in the second paragraph of the Indexing section, which the
	// head-of-chunk preview never reaches.
	resp := search(t, a, "rename")
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidate for a term that is in the corpus")
	}
	c := resp.Candidates[0]
	if !strings.Contains(c.Excerpt, "rename") {
		t.Errorf("excerpt of %s does not contain the term that matched: %q", c.Locator.Local, c.Excerpt)
	}
	if c.ExcerptKind != recall.ExcerptMatched {
		t.Errorf("excerpt kind = %q, want %q", c.ExcerptKind, recall.ExcerptMatched)
	}
	if strings.HasPrefix(c.Excerpt, "An index is a rebuildable projection") {
		t.Errorf("excerpt is still the head of the chunk: %q", c.Excerpt)
	}
	for _, c := range resp.Candidates {
		if n := utf8.RuneCountInString(c.Excerpt); n > 400 {
			t.Errorf("excerpt of %s is %d runes; the bound is 400", c.Locator.Local, n)
		}
	}
}

// A heading immediately followed by another heading is still an addressable
// chunk. It names the document and can be the densest lexical hit, but it has
// no body to answer with. The adapter must keep it reachable and distinguish
// its preview from a later chunk whose body actually matched; fusion uses that
// distinction when choosing which chunk speaks for the document.
func TestBodylessHeadingAndMatchedContentStayDistinguishable(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	dir := filepath.Join(root, "research")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create research directory: %v", err)
	}
	body := "# Retina specialists — drivable range\n\n" +
		"## Boise area\n\n" +
		"### Best first call\n\n" +
		"The practice has a fellowship-trained retina surgeon and an emergency policy.\n"
	if err := os.WriteFile(filepath.Join(dir, "specialists.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write document: %v", err)
	}
	a, _ := newAdapter(t, root, nil)

	resp := search(t, a, "retina")
	var preview, matched bool
	for _, c := range resp.Candidates {
		if c.SourceRecordID != "research/specialists.md" {
			continue
		}
		switch c.ExcerptKind {
		case recall.ExcerptPreview:
			preview = preview || c.Locator.Local == "research/specialists.md#L3-L3"
		case recall.ExcerptMatched:
			matched = matched || strings.Contains(strings.ToLower(c.Excerpt), "retina")
		}
	}
	if !preview || !matched {
		t.Fatalf("retina candidates preview=%v matched=%v; want the body-less heading and content",
			preview, matched)
	}

	// "drivable" appears only in the H1. Descendant chunks inherit that
	// heading path, so the record remains reachable even though none can claim
	// a matched body excerpt.
	headingOnly := search(t, a, "drivable")
	if len(headingOnly.Candidates) == 0 {
		t.Fatal("heading-only term became unreachable")
	}
	if firstFrom(t, headingOnly, "research/specialists.md").ExcerptKind != recall.ExcerptPreview {
		t.Error("heading-only query claimed a matched body excerpt")
	}
}

// TestExcerptIsStableAcrossSearches. Eval runs compare excerpts between runs,
// so the same query against the same generation has to select the same window.
func TestExcerptIsStableAcrossSearches(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, cleanCorpus(t), nil)

	first := search(t, a, "generation rename publishes")
	for i := range 5 {
		again := search(t, a, "generation rename publishes")
		if len(again.Candidates) != len(first.Candidates) {
			t.Fatalf("run %d returned %d candidates, first run returned %d",
				i, len(again.Candidates), len(first.Candidates))
		}
		for j, c := range again.Candidates {
			if c.Excerpt != first.Candidates[j].Excerpt {
				t.Fatalf("run %d, result %d: excerpt %q, first run %q",
					i, j, c.Excerpt, first.Candidates[j].Excerpt)
			}
		}
	}
}

// TestNamedDocumentKeepsTheIndexedPreview. A document the query named outright
// matched no text at all, so the head of the chunk is the honest preview — and
// it has to be reported as a preview rather than as the span that matched.
func TestNamedDocumentKeepsTheIndexedPreview(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	if err := os.MkdirAll(filepath.Join(root, "guides"), 0o755); err != nil {
		t.Fatalf("create guides: %v", err)
	}
	body := "# Welcome\n\nThe kitchen is on the second floor.\n\nCoffee is downstairs.\n"
	if err := os.WriteFile(filepath.Join(root, "guides", "onboarding.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write document: %v", err)
	}
	a, _ := newAdapter(t, root, nil)

	resp := search(t, a, "guides/onboarding.md")
	if len(resp.Candidates) == 0 {
		t.Fatal("naming a document returned nothing")
	}
	c := resp.Candidates[0]
	if !c.Exact() {
		t.Fatalf("top candidate %s is not the named document", c.Locator.Local)
	}
	if c.ExcerptKind != recall.ExcerptPreview {
		t.Errorf("excerpt kind = %q, want %q: nothing in the body matched",
			c.ExcerptKind, recall.ExcerptPreview)
	}
	if !strings.HasPrefix(c.Excerpt, "The kitchen is on the second floor.") {
		t.Errorf("excerpt = %q, want the opening of the named document", c.Excerpt)
	}
}

// TestExcerptClaimsNothingWhenTheChunkChanged. The window is cut from the live
// file, so a document edited after the build could otherwise be quoted at
// offsets this generation never ranked. The indexed excerpt is what is shown
// instead, and no kind is claimed for it: an unreadable body says nothing about
// whether the query matched the record, so calling it a preview would assert
// something the adapter did not establish.
func TestExcerptClaimsNothingWhenTheChunkChanged(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	rewritten := "# Recall Architecture\n\n## Indexing\n\nEverything here is different now.\n"
	path := filepath.Join(root, "projects", "recall", "architecture.md")
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("rewrite document: %v", err)
	}

	resp := search(t, a, "rename")
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidate from the still-published generation")
	}
	c := resp.Candidates[0]
	if c.ExcerptKind != "" {
		t.Errorf("excerpt kind = %q, want no claim for a chunk the file no longer holds",
			c.ExcerptKind)
	}
	if strings.Contains(c.Excerpt, "Everything here is different now") {
		t.Errorf("excerpt quotes text this generation never ranked: %q", c.Excerpt)
	}
	if !strings.HasPrefix(c.Excerpt, "An index is a rebuildable projection") {
		t.Errorf("excerpt = %q, want the indexed excerpt", c.Excerpt)
	}
	if got := resp.Diagnostics["excerpt_basis_unavailable"]; got != 1 {
		t.Errorf("diagnostics report %v results with no excerpt basis, want 1", got)
	}
}

// TestExcerptRefusesAReflowedBody is the determinism criterion at the seam it
// is easiest to lose. The content fingerprint is normalized over tokens, so a
// paragraph rewrapped with different punctuation is the same fingerprint and a
// different set of line offsets — a fingerprint gate would accept it and cut a
// different window for the same query against the same generation, while the
// candidate still reported that generation's watermark.
func TestExcerptRefusesAReflowedBody(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	path := filepath.Join(root, "projects", "recall", "architecture.md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	before := search(t, a, "rename")
	if len(before.Candidates) == 0 {
		t.Fatal("no candidate for a term that is in the corpus")
	}

	// Same tokens in the same order, rewrapped: one line instead of two, and a
	// comma moved. Nothing a token-normalized hash can see.
	reflowed := strings.Replace(string(original),
		"The builder writes a whole generation and publishes it with a single rename, so\n"+
			"an interrupted build costs freshness and nothing else. Example of the layout:",
		"The builder writes a whole generation and publishes it with a single rename "+
			"so an interrupted build costs freshness and nothing else — example of the layout:",
		1)
	if reflowed == string(original) {
		t.Fatal("fixture text moved; the reflow no longer applies")
	}
	if err := os.WriteFile(path, []byte(reflowed), 0o644); err != nil {
		t.Fatalf("reflow document: %v", err)
	}

	after := search(t, a, "rename")
	if len(after.Candidates) == 0 {
		t.Fatal("no candidate after the reflow")
	}
	got := after.Candidates[0]
	if got.ContentFingerprint != before.Candidates[0].ContentFingerprint {
		t.Fatal("the reflow changed the fingerprint; it no longer tests the gate it was written for")
	}
	if got.ExcerptKind != "" {
		t.Errorf("excerpt kind = %q, want no claim for a body that is no longer byte for byte "+
			"what was indexed", got.ExcerptKind)
	}
	if !strings.HasPrefix(got.Excerpt, "An index is a rebuildable projection") {
		t.Errorf("excerpt = %q, want the indexed excerpt", got.Excerpt)
	}
}
