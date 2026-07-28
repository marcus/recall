package docs

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// BM25 parameters. These are the standard values, stated here rather than
// buried: they are ranking configuration, and a future evaluation that moves
// them has to move them somewhere visible.
const (
	bm25K1 = 1.2
	bm25B  = 0.75

	// minimumQueryTerms is how many of a question's content terms a chunk has to
	// carry to be a candidate at all, and the share it carries is what
	// multiplies its score. BM25 has no coordination factor of its own: without
	// one, a chunk holding a single common word of a six-word question is scored
	// against a chunk that answered the whole question, separated only by idf.
	// That is how a sentence came to return four times what the keyword inside
	// it did, on the same question.
	//
	// A count of two, and not a fraction of the query, because both packs say a
	// fraction is wrong in one direction or the other. smoke's "the backup
	// restore runbook for atlas storage" is answered by a chunk carrying two of
	// its six terms — the question names a distractor, and the answer is
	// entitled to ignore it — while the chunks that flood firstuse's "what is
	// the sidecar project for" carry two of three. Half the terms drops the
	// first; a third of them admits the second. What separates a paraphrase from
	// a coincidence is not how much of a question it covers, but whether it
	// covers more of it than any single word would, which is also exactly the
	// reported defect: one term of six admitted the whole corpus.
	//
	// Weighing the terms by idf rather than counting them was measured and
	// rejected: it drops that same smoke case, whose two covered terms are the
	// common ones in that corpus, and the judgments in both packs side with the
	// count. The score being scaled is idf-weighted already.
	//
	// A question with fewer terms than this requires all of them. The terms are
	// the ones that were asked, whether or not this generation indexes them: a
	// requirement computed against the corpus's own vocabulary would quietly
	// become "carry the one word I happen to have" for a question about
	// something the corpus is missing, and a filter narrowing that corpus would
	// change what the same question demanded of it. So "what is the zxqv
	// project", two terms of which this corpus has one, answers nothing instead
	// of answering with everything filed under a project — 64 document results
	// on the home profile before this rule and none after. That is the honest
	// outcome where degrading to the query's weakest surviving word is not, and
	// it is what abstention is for. The recourse is the term on its own or the
	// document's name, both of which say what a question cannot: that the caller
	// meant exactly this much.
	//
	// The floor applies where content eligibility already applied — a question
	// whose grammatical shell was removed — and applying it to bare keywords was
	// measured and reverted. smoke's "signals stream rotation" is answered by a
	// chunk carrying one of those three words, and the pack marks it required:
	// somebody who types three words has typed the alternatives they would
	// accept, while somebody who writes a sentence has written one subject.
	// Coverage still scales a keyword query's scores; it excludes nothing there.
	minimumQueryTerms = 2

	// defaultLimit bounds a search that asked for no limit. The core normally
	// sets one; a document corpus must not flood the pool when it does not.
	defaultLimit = 20
)

// hit is one chunk that answered, with the reasons it did.
type hit struct {
	chunk int
	score float64
	exact bool
	alias bool
}

// searchIndex ranks the generation's chunks against a request.
//
// Ordering is total and deterministic: score, then path, then chunk ordinal.
// Two rebuilds of an unchanged corpus produce identical scores from identical
// term statistics, so the tie-breaks are what make the ORDER identical too —
// without them, equal scores would come out in whatever order the postings
// happened to be visited.
func searchIndex(g *generation, req recall.SearchRequest) ([]hit, queryAnalysis) {
	if !wantsDocuments(req.Filters.RecordTypes) {
		return nil, queryAnalysis{}
	}
	allowed := docFilter(g, req)
	query := analyzeQuery(req.Query)
	query = preserveRankingAfterContentMatch(g, query)

	scores := map[int]float64{}
	if len(query.terms) > 0 && len(g.chunks) > 0 {
		scoreBM25(g, uniqueTerms(query.terms), allowed, scores)
	}

	// Exact identifiers deliberately use the raw token stream. A path, alias,
	// or title can contain an English function word, and exact lookup must not
	// change because lexical prose normalization did.
	exact, alias := identifierMatches(g, query.raw)

	// One coverage measurement per chunk, used twice: as the admission floor and
	// as the coordination factor BM25 does not have. Both read the content
	// terms, so scaffolding pays for neither.
	coverage := newQueryCoverage(query)

	// An exact identifier match must surface the document even when its text
	// shares no term with the query: "docs/spec.md" names a file rather than
	// describing it, and a source that answered nothing there would be wrong.
	// Coverage never excludes it for the same reason, and still scales its
	// lexical score, which the exact partition of the sort order outranks.
	// Chunks are visited in index order, which is stable, so the hit list is
	// the same before sorting on every run.
	hits := make([]hit, 0, len(scores))
	for i, c := range g.chunks {
		_, scored := scores[i]
		if !scored && !exact[c.Path] {
			continue
		}
		covered := coverage.covered(c)
		if scored && !exact[c.Path] && !coverage.admits(covered) {
			continue
		}
		if !allowed(c) {
			continue
		}
		score := scores[i] * coverage.share(covered)
		hits = append(hits, hit{chunk: i, score: score, exact: exact[c.Path], alias: alias[c.Path]})
	}

	sort.Slice(hits, func(a, b int) bool {
		x, y := hits[a], hits[b]
		if x.exact != y.exact {
			return x.exact
		}
		if x.score != y.score {
			return x.score > y.score
		}
		cx, cy := g.chunks[x.chunk], g.chunks[y.chunk]
		if cx.Path != cy.Path {
			return cx.Path < cy.Path
		}
		return cx.Ord < cy.Ord
	})
	return hits, query
}

// preserveRankingAfterContentMatch keeps the baseline's full-query ranking
// when at least one meaningful term reaches the index. Function words are
// excluded only from proving relevance by themselves: if every content term is
// absent, they cannot manufacture candidates; if content is present, the full
// query may still order those results exactly as it did before normalization.
//
// Candidate admission separately requires more than one of the query's content
// terms on that same chunk: see minimumQueryTerms. Scaffolding can therefore
// influence order among content-bearing candidates, but can never create one,
// and cannot pay any part of what admission costs.
func preserveRankingAfterContentMatch(g *generation, query queryAnalysis) queryAnalysis {
	if !query.normalized {
		return query
	}
	for _, term := range query.terms {
		if len(g.postings[term]) == 0 {
			continue
		}
		query.terms = append(query.terms[:0], query.raw...)
		query.scoringWithScaffolding = true
		return query
	}
	return query
}

// contentTerms are the terms a candidate has to answer for: the retained ones
// when normalization removed anything, the whole query otherwise. It is the
// population query_term_count reports and the one excerpts are anchored on, so
// the coverage a result is admitted and scored under is the coverage the
// diagnostics describe.
func contentTerms(query queryAnalysis) []string {
	if query.normalized {
		return query.retained
	}
	return query.terms
}

// queryCoverage measures one query against the chunks that will answer it.
//
// It reads the query and nothing else. A rule that consulted the index would
// state a different requirement for the same question depending on what the
// corpus happens to hold, and — because filters are applied per chunk after
// scoring — a different one again for the same corpus under a project or time
// filter. The requirement is a property of what was asked.
type queryCoverage struct {
	terms    []string
	required int
}

func newQueryCoverage(query queryAnalysis) queryCoverage {
	cov := queryCoverage{terms: uniqueTerms(contentTerms(query))}
	if query.normalized {
		cov.required = min(len(cov.terms), minimumQueryTerms)
	}
	return cov
}

func (c queryCoverage) covered(chunk indexedChunk) int {
	n := 0
	for _, term := range c.terms {
		if chunk.Terms[term] > 0 {
			n++
		}
	}
	return n
}

func (c queryCoverage) admits(covered int) bool {
	return covered >= c.required
}

// share is the coordination factor. An empty query is covered by everything:
// nothing is being measured, and a zero here would silently score a whole
// source to nothing.
func (c queryCoverage) share(covered int) float64 {
	if len(c.terms) == 0 {
		return 1
	}
	return float64(covered) / float64(len(c.terms))
}

// scoreBM25 accumulates Okapi BM25 over the postings.
//
//	idf(t)   = ln(1 + (N - n(t) + 0.5) / (n(t) + 0.5))
//	score(c) = sum over query terms of idf(t) * tf / (tf + k1*(1 - b + b*len/avg))
//
// It is written out here rather than pulled in as a dependency: this is the
// whole of a lexical baseline, and a baseline the project cannot read is not a
// baseline it can defend.
func scoreBM25(g *generation, terms []string, allowed func(indexedChunk) bool, out map[int]float64) {
	n := float64(len(g.chunks))
	for _, term := range terms {
		postings := g.postings[term]
		if len(postings) == 0 {
			continue
		}
		df := float64(len(postings))
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for _, p := range postings {
			c := g.chunks[p.chunk]
			if !allowed(c) {
				continue
			}
			tf := float64(p.tf)
			norm := 1 - bm25B + bm25B*float64(c.Length)/g.avgLen
			out[p.chunk] += idf * tf / (tf + bm25K1*norm)
		}
	}
}

// uniqueTerms keeps each query term once. Repeating a word in a query is
// emphasis a lexical index has no honest way to interpret, and counting it
// twice would silently double that term's weight.
func uniqueTerms(tokens []string) []string {
	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// identifierMatches finds documents the query named outright.
func identifierMatches(g *generation, tokens []string) (exact, alias map[string]bool) {
	exact, alias = map[string]bool{}, map[string]bool{}
	if len(tokens) == 0 {
		return exact, alias
	}
	for _, d := range g.docs {
		for _, id := range g.idents[d.Path] {
			if !containsSequence(tokens, id.tokens) {
				continue
			}
			exact[d.Path] = true
			if id.alias {
				alias[d.Path] = true
			}
		}
	}
	return exact, alias
}

func wantsDocuments(types []recall.RecordType) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if t == recall.RecordDocument {
			return true
		}
	}
	return false
}

// docFilter turns the request's filters into a predicate.
//
// Time filters and as_of both apply to the document's modification time, which
// is the only temporal fact a file carries. as_of is honored as a filter and
// never as a snapshot: a document modified after the boundary is excluded
// entirely, because its earlier content is not something this adapter can
// reconstruct. Claiming otherwise would answer a historical question from
// current state.
func docFilter(g *generation, req recall.SearchRequest) func(indexedChunk) bool {
	f := req.Filters
	entities := make([][]string, 0, len(f.Entities))
	for _, e := range f.Entities {
		if toks := tokenize(e); len(toks) > 0 {
			entities = append(entities, toks)
		}
	}

	docOK := func(d indexedDoc) bool {
		switch {
		case f.Since != nil && d.ModTime.Before(*f.Since):
			return false
		case f.Until != nil && d.ModTime.After(*f.Until):
			return false
		case req.AsOf != nil && d.ModTime.After(*req.AsOf):
			return false
		case f.Project != "" && !strings.EqualFold(f.Project, d.Project):
			return false
		}
		return true
	}

	cache := make(map[string]bool, len(g.docs))
	return func(c indexedChunk) bool {
		ok, known := cache[c.Path]
		if !known {
			d, found := g.doc(c.Path)
			ok = found && docOK(d)
			cache[c.Path] = ok
		}
		if !ok {
			return false
		}
		// Entity scope is applied lexically, which is all a document corpus
		// can honestly do: every token of every named entity must appear in the
		// chunk. It is a conjunctive term filter, not entity resolution, and the
		// search diagnostics say so.
		for _, tokens := range entities {
			for _, t := range tokens {
				if c.Terms[t] == 0 {
					return false
				}
			}
		}
		return true
	}
}

// candidates renders ranked hits as the envelope the core consumes, and reports
// how many of them could not be read from the corpus to select an excerpt.
//
// The excerpt is chosen here rather than at index time, so it can be the span
// the query matched. bodies reads the live document for that, at most once per
// distinct document in the truncated result list; see excerpt.go. A nil bodies
// reads nothing, and every excerpt is the indexed one with no claim about it.
func candidates(
	ctx context.Context,
	g *generation,
	sourceID string,
	hits []hit,
	limit int,
	query queryAnalysis,
	bodies *bodyReader,
) ([]recall.Candidate, int) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	confirmed := g.confirmedAt()
	observed := g.header.BuiltAt
	terms := excerptTerms(query)
	unavailable := 0

	out := make([]recall.Candidate, 0, len(hits))
	for i, h := range hits {
		c := g.chunks[h.chunk]
		doc, _ := g.doc(c.Path)
		score := h.score
		event := doc.ModTime

		signals := []recall.MatchSignal{recall.MatchLexical}
		if h.exact {
			signals = append(signals, recall.MatchExactIdentifier)
		}
		if h.alias {
			signals = append(signals, recall.MatchAlias)
		}

		// An excerpt the adapter could not verify against the corpus asserts
		// nothing. Calling it a preview would claim the query matched nothing in
		// the record, which is not what an unreadable file says.
		excerpt, kind := c.Excerpt, recall.ExcerptKind("")
		switch window, basis := bodies.excerpt(ctx, c, terms); basis {
		case basisMatched:
			excerpt, kind = window, recall.ExcerptMatched
		case basisNoMatch:
			kind = recall.ExcerptPreview
		default:
			unavailable++
		}

		out = append(out, recall.Candidate{
			CandidateID: chunkCandidateID(c),
			// The identity of the DOCUMENT. Two chunks of one file are one
			// thing said once, and corroboration collapses them on this value.
			SourceRecordID: doc.RecordID(),
			Locator:        recall.Locator{SourceID: sourceID, Local: c.Local()},
			RecordType:     recall.RecordDocument,
			Title:          chunkTitle(doc, c),
			Excerpt:        excerpt,
			ExcerptKind:    kind,
			LocalRank:      i + 1,
			LocalScore:     &score,
			MatchSignals:   signals,
			ObservedAt:     &observed,
			ConfirmedAt:    confirmed,
			EventTime:      &event,
			SourceRevision: g.header.Watermark,
			Sensitivity:    recall.SensitivityInternal,
			Metadata: map[string]any{
				"path":         c.Path,
				"heading":      c.Heading,
				"heading_path": c.HeadingPath,
				"start_line":   c.StartLine,
				"end_line":     c.EndLine,
				"chunk_index":  c.Ord,
				"chunk_count":  doc.ChunkCount,
				"doc_title":    doc.Title,
				"project":      doc.Project,
				"generation":   g.id,
			},
			ContentFingerprint: c.Fingerprint,
		})
	}
	return out, unavailable
}

// chunkCandidateID is stable within a generation and survives edits that move
// lines around, which the locator cannot.
func chunkCandidateID(c indexedChunk) string {
	return c.Path + "#c" + strconv.Itoa(c.Ord)
}

// chunkTitle names the section without repeating the document title when the
// document's own H1 already opens the heading path.
func chunkTitle(doc indexedDoc, c indexedChunk) string {
	path := c.HeadingPath
	if len(path) > 0 && path[0] == doc.Title {
		path = path[1:]
	}
	if len(path) == 0 {
		return doc.Title
	}
	return doc.Title + " > " + strings.Join(path, " > ")
}

// searchDiagnostics reports what actually ran, so a thin result is
// distinguishable from a misrouted query.
func searchDiagnostics(
	g *generation,
	req recall.SearchRequest,
	query queryAnalysis,
	pool int,
	unreadable int,
	elapsed time.Duration,
) map[string]any {
	diag := map[string]any{
		"query_mode":       string(recall.QueryLexical),
		"query_analyzer":   queryAnalyzer,
		"query_term_count": queryRetainedTermCount(query),
		"generation":       g.id,
		"pool_size":        pool,
		"indexed_count":    len(g.docs),
		"chunk_count":      len(g.chunks),
		"elapsed_ms":       elapsed.Milliseconds(),
	}
	if query.normalized {
		diag["query_terms_removed"] = query.removed
		// How many of the query's terms a candidate had to carry. A question
		// that admitted less than the caller expected is then a stated rule
		// rather than a thin corpus.
		diag["query_terms_required"] = newQueryCoverage(query).required
	}
	if query.scoringWithScaffolding {
		diag["query_scoring"] = "full_query_over_content_candidates"
	}
	if len(g.failures) > 0 {
		diag["failed_count"] = len(g.failures)
	}
	if unreadable > 0 {
		// Results whose document could not be read to select an excerpt: gone,
		// unreadable, grown past the corpus boundary, or no longer holding the
		// bytes this generation ranked. Their excerpts are the indexed ones and
		// claim nothing, and the count is what makes a corpus drifting under a
		// published generation visible without diffing it by hand.
		diag["excerpt_basis_unavailable"] = unreadable
	}
	if len(req.Filters.Entities) > 0 {
		diag["entity_filter"] = "lexical_tokens"
	}
	if req.AsOf != nil {
		diag["as_of"] = "filter_on_mtime"
	}
	return diag
}

func queryRetainedTermCount(query queryAnalysis) int {
	return len(uniqueTerms(contentTerms(query)))
}

// excerptTerms are the terms an excerpt window may be anchored on.
//
// It is the same population query_term_count reports, so the query the
// diagnostics describe is the query the excerpt was selected against. Function
// words are excluded for the same reason they are excluded from proving
// relevance: a window anchored on "the" shows a match nobody asked about.
func excerptTerms(query queryAnalysis) map[string]bool {
	terms := contentTerms(query)
	out := make(map[string]bool, len(terms))
	for _, t := range terms {
		out[t] = true
	}
	return out
}
