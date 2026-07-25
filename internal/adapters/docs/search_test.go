package docs_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
