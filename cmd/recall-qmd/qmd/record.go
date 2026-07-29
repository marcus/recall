package qmd

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/marcus/recall/pkg/recall"
)

// searchHit is one element of the array `qmd --json` writes.
type searchHit struct {
	// DocID is qmd's content hash for the document, written with a leading '#'.
	DocID string `json:"docid"`
	// Score is qmd's own number. Its scale differs per mode and the reranked
	// modes saturate it — an off-corpus document scores what a relevant one
	// does — so it is diagnostic only and never becomes relevance.
	Score float64 `json:"score"`
	// File is `qmd://<collection>/<relative path>`.
	File string `json:"file"`
	// Line is the line qmd considers the hit's anchor. The snippet header is
	// what states the span, and the two are not always the same number.
	Line int `json:"line"`
	// Title is the document's title as qmd extracted it.
	Title string `json:"title"`
	// Snippet is a header line stating the span, then the span's text.
	Snippet string `json:"snippet"`
	// Explain is present when the invocation asked for it, which the hybrid and
	// full modes always do.
	Explain *explain `json:"explain,omitempty"`
}

// explain is qmd's per-result score trace. It is the whole reason a layered
// mode is attributable at all: it names every expanded query, which backend
// produced which list, each list's rank and weight, and the reranker's own
// score.
type explain struct {
	FTSScores    []float64 `json:"ftsScores"`
	VectorScores []float64 `json:"vectorScores"`
	RRF          struct {
		Rank          int            `json:"rank"`
		PositionScore float64        `json:"positionScore"`
		Weight        float64        `json:"weight"`
		BaseScore     float64        `json:"baseScore"`
		TopRankBonus  float64        `json:"topRankBonus"`
		TotalScore    float64        `json:"totalScore"`
		Contributions []contribution `json:"contributions"`
	} `json:"rrf"`
	RerankScore  float64 `json:"rerankScore"`
	BlendedScore float64 `json:"blendedScore"`
}

type contribution struct {
	ListIndex       int     `json:"listIndex"`
	Source          string  `json:"source"`
	QueryType       string  `json:"queryType"`
	Query           string  `json:"query"`
	Rank            int     `json:"rank"`
	Weight          float64 `json:"weight"`
	BackendScore    float64 `json:"backendScore"`
	RRFContribution float64 `json:"rrfContribution"`
}

// Bounds on what a candidate carries. A candidate is a pointer, and the
// per-component signal list is diagnostic detail rather than payload.
const (
	excerptBytes       = 480
	maxComponents      = 8
	maxComponentQuery  = 120
	maxExpandedQueries = 8
)

// snippetHeader matches the line qmd puts at the top of every snippet:
//
//	@@ -17,4 @@ (16 before, 88 after)
//
// The first pair is the span — start line and line count — and the parenthesis
// states how much of the document lies on either side of it. The span is what a
// locator is minted from, so this parse is what makes expansion possible at all.
var snippetHeader = regexp.MustCompile(`^@@ -(\d+),(\d+) @@(?: \((\d+) before, (\d+) after\))?`)

// span is a document and the line range one hit points at.
type span struct {
	Path   string
	Start  int
	End    int
	Before int
	After  int
	// Stated reports whether the range came from the snippet header. When it
	// did not, the range was derived from the anchor line and the snippet's own
	// line count, which is a weaker claim and is recorded in metadata.
	Stated bool
}

// Local renders the locator's local part: path plus line range, which is
// exactly what expansion needs and what a person can act on by hand. It is the
// same spelling the built-in lexical document adapter uses, so a locator from
// either source reads the same way.
func (s span) Local() string {
	return fmt.Sprintf("%s#L%d-L%d", s.Path, s.Start, s.End)
}

// errForeignCollection reports a result naming a collection this source does
// not serve.
//
// Every search passes `-c <collection>`, so this should be unreachable. It is
// checked anyway because the alternative to checking is emitting a candidate
// whose source_record_id, locator, and expansion all name a file in a corpus
// this instance was never configured to read. A result that trips it is dropped
// and counted, and the count makes the search partial: something matched that
// this source cannot account for, and it cannot say what else it missed.
var errForeignCollection = errors.New("qmd: result names another collection")

// parseHit turns one qmd result into a document, a span, and clean text.
func parseHit(hit searchHit, collection string) (span, string, string, error) {
	const scheme = "qmd://"
	file := strings.TrimSpace(hit.File)
	if !strings.HasPrefix(file, scheme) {
		return span{}, "", "", fmt.Errorf("%w: result file %q is not a qmd:// reference",
			errBrokenContract, sanitizeLine(file))
	}
	name, rel, found := strings.Cut(strings.TrimPrefix(file, scheme), "/")
	if !found || name == "" || rel == "" {
		return span{}, "", "", fmt.Errorf("%w: result file %q names no document",
			errBrokenContract, sanitizeLine(file))
	}
	if name != collection {
		return span{}, "", "", fmt.Errorf("%w: %q", errForeignCollection, sanitizeLine(name))
	}
	clean := path.Clean(rel)
	if clean != rel || path.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		// A path that is not in normal form, or that leaves the corpus, would
		// make a locator that reads a file outside the tree this source was
		// configured for.
		return span{}, "", "", fmt.Errorf("%w: result path %q is not inside the collection",
			errBrokenContract, sanitizeLine(rel))
	}

	header, body := splitSnippet(hit.Snippet)
	got := span{Path: clean}
	if header != nil {
		got.Start, got.End = header.Start, header.End
		got.Before, got.After = header.Before, header.After
		got.Stated = true
	} else {
		// No header. The anchor line is the only statement about position left,
		// and the span is as many lines as the snippet carries — which is a
		// claim about the snippet rather than about the document, so Stated
		// stays false and metadata says so.
		got.Start = max(1, hit.Line)
		got.End = got.Start + max(0, countLines(body)-1)
	}
	if got.Start < 1 {
		got.Start = 1
	}
	if got.End < got.Start {
		got.End = got.Start
	}

	title := sanitizeLine(hit.Title)
	if title == "" {
		title = path.Base(clean)
	}
	return got, title, sanitizeBlock(body), nil
}

type headerSpan struct {
	Start, End, Before, After int
}

// splitSnippet separates the span header from the span text.
func splitSnippet(snippet string) (*headerSpan, string) {
	first, rest, found := strings.Cut(snippet, "\n")
	match := snippetHeader.FindStringSubmatch(strings.TrimSpace(first))
	if match == nil {
		return nil, snippet
	}
	start, err1 := strconv.Atoi(match[1])
	count, err2 := strconv.Atoi(match[2])
	if err1 != nil || err2 != nil || start < 1 {
		return nil, snippet
	}
	if count < 1 {
		count = 1
	}
	got := &headerSpan{Start: start, End: start + count - 1}
	if match[3] != "" {
		got.Before, _ = strconv.Atoi(match[3])
	}
	if match[4] != "" {
		got.After, _ = strconv.Atoi(match[4])
	}
	if !found {
		return got, ""
	}
	return got, rest
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// candidateOf fills the envelope for one hit.
func candidateOf(hit searchHit, got span, title, body string, sourceID string,
	rank int, set Settings, terms []string, revision string) recall.Candidate {

	score := hit.Score
	relevance := spanRelevance(terms, title, body)
	docID := strings.TrimPrefix(strings.TrimSpace(hit.DocID), "#")

	candidate := recall.Candidate{
		// The hit: one span of one document. Two spans of one file are two
		// candidates and one record.
		CandidateID: candidateID(docID, got),
		// The identity of the DOCUMENT, spelled as the collection-relative
		// path — which is the same value the built-in lexical document adapter
		// derives for the same file. That is deliberate and load-bearing: the
		// two sources are meant to run over one corpus, and corroboration
		// collapses on this value, so a per-span identity would let this source
		// corroborate itself with two halves of one section and a differently
		// spelled one would let two sources corroborate each other for
		// agreeing about a file they both read.
		SourceRecordID: got.Path,
		Locator:        recall.Locator{SourceID: sourceID, Local: got.Local()},
		RecordType:     recall.RecordDocument,
		Title:          title,
		Excerpt:        clip(body, excerptBytes),
		// The snippet IS the span qmd matched, cut by qmd around the hit rather
		// than taken from the head of the document, so it is evidence of the
		// match and says so.
		ExcerptKind:  recall.ExcerptMatched,
		LocalRank:    rank,
		LocalScore:   &score,
		Relevance:    &relevance,
		MatchSignals: signalsOf(hit, set.Mode),
		// No event time. The only date available for a file is its mtime, which
		// is a property of this checkout rather than of the record: the same
		// corpus on two machines would carry two different event times.
		SourceRevision: revision,
		Sensitivity:    recall.SensitivityInternal,
		Metadata:       metadataOf(hit, got, set),
		// Built from the document's own content identity and from nothing about
		// where this instance found it: qmd's docid is a hash of the document.
		// Two instances configured over one collection therefore produce equal
		// fingerprints for one document, and a duplicate configuration is
		// harmless for corroboration until an operator fixes it.
		ContentFingerprint: contentFingerprint(docID),
	}
	return candidate
}

func candidateID(docID string, got span) string {
	if docID == "" {
		docID = "nodoc"
	}
	return fmt.Sprintf("%s@L%d-L%d", docID, got.Start, got.End)
}

func contentFingerprint(docID string) string {
	if docID == "" {
		return ""
	}
	return "qmd-docid:" + docID
}

// spanRelevance is [recall.Candidate.Relevance] for one hit, recomputed here
// rather than taken from qmd's score.
//
// qmd's number cannot be it. It is ordinal, its scale differs per mode, and in
// the reranked modes it saturates: an off-corpus query ("sourdough starter
// hydration") returns an unrelated travel note at the same 0.88 a genuinely
// relevant document earns, and `--min-score` cannot express an admission floor
// over that. Relevance is the one number compared across sources and the thing
// an abstention decision rests on, so it is measured on the definition every
// source shares, over the text this source actually has.
//
// What this source has is the returned span, not the whole chunk qmd ranked.
// The measurement is therefore honest about a smaller record than the one that
// matched: concentration is computed over the span's own length, which is the
// text the match was found in, and a span is denser than the section around it.
// The alternative — padding the length with text no query term could reach —
// is the error the definition explicitly forbids, and reading every file at
// search time to recover the full chunk would put the corpus back on the
// query path this adapter exists to keep off it. The consequence is stated in
// doc.go: against the lexical document adapter over one corpus, this source's
// relevance runs slightly high on long sections.
//
// It is never nil. A source that omits the field is read as 1.0, the maximum,
// so an omitting source outranks every source that reports honestly.
func spanRelevance(terms []string, title, body string) float64 {
	tokens := tokenize(title + "\n" + body)
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return recall.RelevanceOverCounts(terms, counts, len(tokens))
}

// signalsOf names why this candidate was returned, from qmd's own trace where
// there is one.
//
// It is derived from the score trace rather than from the mode, because the mode
// says which backends COULD have contributed and the trace says which did. A
// hybrid result that only the FTS list produced matched lexically, and calling
// it semantic would credit the embedding model with a keyword hit.
func signalsOf(hit searchHit, mode Mode) []recall.MatchSignal {
	lexical, semantic := false, false
	if hit.Explain != nil {
		lexical = len(hit.Explain.FTSScores) > 0
		semantic = len(hit.Explain.VectorScores) > 0
		for _, c := range hit.Explain.RRF.Contributions {
			switch c.Source {
			case "fts", "lex":
				lexical = true
			case "vec", "vector":
				semantic = true
			}
		}
	}
	if !lexical && !semantic {
		// No trace to read: the bm25 and vector modes do not produce one, and a
		// hybrid result without an explain block cannot be attributed, so the
		// mode's own retrieval is the honest answer.
		switch mode {
		case ModeBM25:
			lexical = true
		case ModeVector:
			semantic = true
		default:
			lexical, semantic = true, true
		}
	}
	out := make([]recall.MatchSignal, 0, 2)
	if lexical {
		out = append(out, recall.MatchLexical)
	}
	if semantic {
		out = append(out, recall.MatchSemantic)
	}
	return out
}

// metadataOf carries the per-result attribution.
//
// Everything qmd knows about why this result ranked where it did goes here, per
// candidate rather than per run, because a per-run aggregate cannot say which
// layer fixed or broke one query. Every string is sanitized and every list is
// bounded: the expanded queries are LLM output about source text, the core's
// sanitizer walks only top-level fields, and this one is nested.
func metadataOf(hit searchHit, got span, set Settings) map[string]any {
	md := map[string]any{
		"path":       got.Path,
		"start_line": got.Start,
		"end_line":   got.End,
		"mode":       string(set.Mode),
		"collection": set.Collection,
		"qmd_score":  hit.Score,
	}
	if !got.Stated {
		// The span was inferred from the anchor line rather than stated by qmd.
		// A caller comparing an excerpt with an expansion deserves to know.
		md["span_inferred"] = true
	}
	if got.Stated && (got.Before > 0 || got.After > 0) {
		md["context_before"] = got.Before
		md["context_after"] = got.After
	}
	if hit.Line > 0 {
		md["anchor_line"] = hit.Line
	}
	if hit.Explain == nil {
		return md
	}

	signals := map[string]any{
		"rrf_rank":  hit.Explain.RRF.Rank,
		"rrf_score": hit.Explain.RRF.TotalScore,
	}
	if hit.Explain.RRF.TopRankBonus != 0 {
		signals["rrf_top_rank_bonus"] = hit.Explain.RRF.TopRankBonus
	}
	if set.Mode.Reranks() {
		// Only for a mode that ran the reranker. qmd reports zero when it did
		// not, and a zero would read as "the reranker scored this at nothing".
		signals["rerank_score"] = hit.Explain.RerankScore
		signals["blended_score"] = hit.Explain.BlendedScore
	}
	if len(hit.Explain.FTSScores) > 0 {
		signals["fts_scores"] = boundedFloats(hit.Explain.FTSScores)
	}
	if len(hit.Explain.VectorScores) > 0 {
		signals["vector_scores"] = boundedFloats(hit.Explain.VectorScores)
	}
	if components := boundedComponents(hit.Explain.RRF.Contributions); len(components) > 0 {
		signals["components"] = components
	}
	md["signals"] = signals
	return md
}

func boundedFloats(values []float64) []float64 {
	if len(values) > maxComponents {
		values = values[:maxComponents]
	}
	return append([]float64(nil), values...)
}

func boundedComponents(contributions []contribution) []map[string]any {
	if len(contributions) > maxComponents {
		contributions = contributions[:maxComponents]
	}
	out := make([]map[string]any, 0, len(contributions))
	for _, c := range contributions {
		out = append(out, map[string]any{
			"source":           sanitizeLine(c.Source),
			"query_type":       sanitizeLine(c.QueryType),
			"query":            clip(sanitizeLine(c.Query), maxComponentQuery),
			"rank":             c.Rank,
			"weight":           c.Weight,
			"backend_score":    c.BackendScore,
			"rrf_contribution": c.RRFContribution,
		})
	}
	return out
}

// expandedQueries collects the query strings qmd's expansion layer produced,
// across every result, deduplicated and in first-seen order.
//
// It belongs in the search's diagnostics rather than on a candidate: it is a
// property of the request, and it is the evidence that says whether expansion
// fired and what it fired with. Without it a mode comparison can see that
// `full` beat `hybrid` and not that expansion invented the term that did it.
func expandedQueries(hits []searchHit, original string) []map[string]any {
	seen := map[string]bool{sanitizeLine(original): true}
	out := make([]map[string]any, 0, maxExpandedQueries)
	for _, hit := range hits {
		if hit.Explain == nil {
			continue
		}
		for _, c := range hit.Explain.RRF.Contributions {
			kind := sanitizeLine(c.QueryType)
			if kind == "original" {
				continue
			}
			text := clip(sanitizeLine(c.Query), maxComponentQuery)
			if text == "" || seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, map[string]any{"type": kind, "query": text})
			if len(out) == maxExpandedQueries {
				return out
			}
		}
	}
	return out
}

// tokenize folds text into lowercase alphanumeric tokens.
//
// It is a copy of the rule the built-in lexical document adapter uses, and the
// copy is deliberate rather than lazy: relevance is comparable across sources
// only because every source measures it over the same notion of a term, so
// these two implementations must not drift. An external adapter cannot import
// the built-in's internal package, so the constraint is stated here instead.
// There is no stemming and no synonym expansion: qmd owns query expansion, and
// this side of it only has to count.
func tokenize(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		token := b.String()
		b.Reset()
		if len([]rune(token)) <= maxTokenRunes {
			out = append(out, token)
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
}

// maxTokenRunes bounds a single token: anything longer is a checksum or a
// minified line rather than a word, and no query term can reach it.
const maxTokenRunes = 64

// queryTerms are the distinct terms a relevance denominator is measured
// against.
//
// The function-word list is the same narrow one the built-in lexical document
// adapter applies — articles, copular and do-auxiliaries, and interrogatives,
// the grammatical shell that turns keywords into an English question — for the
// same reason the tokenizer is: coverage is a fraction whose denominator is
// "distinct retained query terms", and two sources retaining different terms
// report two numbers that cannot be compared. A query made entirely of function
// words falls back to its raw terms rather than becoming a query with nothing to
// be about.
func queryTerms(query string) []string {
	raw := tokenize(query)
	terms := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, token := range raw {
		if seen[token] || functionWord(token) {
			continue
		}
		seen[token] = true
		terms = append(terms, token)
	}
	if len(terms) > 0 {
		return terms
	}
	for _, token := range raw {
		if !seen[token] {
			seen[token] = true
			terms = append(terms, token)
		}
	}
	return terms
}

func functionWord(token string) bool {
	switch token {
	case "a", "an", "the",
		"am", "are", "be", "been", "being", "did", "do", "does", "is", "was", "were",
		"what", "when", "where", "which", "who", "whom", "whose", "why", "how":
		return true
	default:
		return false
	}
}
