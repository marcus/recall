package docs

import (
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
func searchIndex(g *generation, req recall.SearchRequest) []hit {
	if !wantsDocuments(req.Filters.RecordTypes) {
		return nil
	}
	allowed := docFilter(g, req)
	tokens := tokenize(req.Query)

	scores := map[int]float64{}
	if len(tokens) > 0 && len(g.chunks) > 0 {
		scoreBM25(g, uniqueTerms(tokens), allowed, scores)
	}

	exact, alias := identifierMatches(g, tokens)

	// An exact identifier match must surface the document even when its text
	// shares no term with the query: "docs/spec.md" names a file rather than
	// describing it, and a source that answered nothing there would be wrong.
	// Chunks are visited in index order, which is stable, so the hit list is
	// the same before sorting on every run.
	hits := make([]hit, 0, len(scores))
	for i, c := range g.chunks {
		_, scored := scores[i]
		if !scored && !exact[c.Path] {
			continue
		}
		if !allowed(c) {
			continue
		}
		hits = append(hits, hit{chunk: i, score: scores[i], exact: exact[c.Path], alias: alias[c.Path]})
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
	return hits
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

// candidates renders ranked hits as the envelope the core consumes.
func candidates(g *generation, sourceID string, hits []hit, limit int) []recall.Candidate {
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	confirmed := g.confirmedAt()
	observed := g.header.BuiltAt

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

		out = append(out, recall.Candidate{
			CandidateID: chunkCandidateID(c),
			// The identity of the DOCUMENT. Two chunks of one file are one
			// thing said once, and corroboration collapses them on this value.
			SourceRecordID: doc.RecordID(),
			Locator:        recall.Locator{SourceID: sourceID, Local: c.Local()},
			RecordType:     recall.RecordDocument,
			Title:          chunkTitle(doc, c),
			Excerpt:        c.Excerpt,
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
	return out
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
func searchDiagnostics(g *generation, req recall.SearchRequest, pool int, elapsed time.Duration) map[string]any {
	diag := map[string]any{
		"query_mode":    string(recall.QueryLexical),
		"generation":    g.id,
		"pool_size":     pool,
		"indexed_count": len(g.docs),
		"chunk_count":   len(g.chunks),
		"elapsed_ms":    elapsed.Milliseconds(),
	}
	if len(g.failures) > 0 {
		diag["failed_count"] = len(g.failures)
	}
	if len(req.Filters.Entities) > 0 {
		diag["entity_filter"] = "lexical_tokens"
	}
	if req.AsOf != nil {
		diag["as_of"] = "filter_on_mtime"
	}
	return diag
}
