package stream

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/recall"
)

// excerptBytes bounds a candidate's preview. A candidate is a pointer, not a
// payload; the locator is how a caller gets the rest.
const excerptBytes = 240

// Search returns this source's own ranked candidates.
//
// Ranking is deliberately simple and explainable:
//
//  1. Exact identifier hits first — a query token equal to a record id, an
//     upstream ref, a correlation key, or a locator's local part. This is a
//     partition, not a bonus, mirroring the core's own exact-match promotion.
//  2. Everything else by term coverage over the title (1.0), the record's own
//     fields — kind, system, actor, ref (0.5) — and the body (0.4).
//  3. Ties broken by newest event first, then by id, so the order is total and
//     reproducible.
//
// A query with no terms is a time-window browse, which is a real question to
// ask an event stream: the window's newest events, in order.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, sourceID, floor, err := a.session()
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	if resp, skipped := adapter.UnsupportedFilters(req.Filters, "entities", "project"); skipped {
		return resp, nil
	}
	if err := stall(ctx, set.StallMS); err != nil {
		return adapter.FailedSearch(err), err
	}

	started := a.now()
	snap, err := a.current(false)
	if err != nil {
		// An unreadable source is never a search that succeeded with no
		// matches. The error carries the code; the response carries the
		// outcome for a caller holding both.
		return adapter.FailedSearch(err), err
	}

	terms := tokenize(req.Query)
	// Resolved once per search over the whole snapshot, never per record: see
	// [recall.ResolveTermVariants] for why the gate is store-wide.
	variants := recall.ResolveTermVariants(terms, snap.holds)
	var (
		hits     []hit
		anyExact bool
	)
	for _, rec := range snap.records {
		if err := ctx.Err(); err != nil {
			return adapter.FailedSearch(err), err
		}
		if !inWindow(rec, req) || !wantedType(rec, req.Filters.RecordTypes) {
			continue
		}
		score, exact := match(rec, terms, variants)
		if len(terms) > 0 && score == 0 && !exact {
			continue
		}
		anyExact = anyExact || exact
		hits = append(hits, hit{rec: rec, score: score, exact: exact})
	}
	matched := len(hits)

	sort.SliceStable(hits, func(i, j int) bool {
		l, r := hits[i], hits[j]
		switch {
		case l.exact != r.exact:
			return l.exact
		case l.score != r.score:
			return l.score > r.score
		case !l.rec.eventTime.Equal(r.rec.eventTime):
			return l.rec.eventTime.After(r.rec.eventTime)
		default:
			return l.rec.id < r.rec.id
		}
	})

	limit := set.maxCandidates()
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	p := pass{snap: snap, set: set, sourceID: sourceID, floor: floor, termList: terms, variants: variants}
	candidates := make([]recall.Candidate, 0, len(hits))
	for i, h := range hits {
		candidates = append(candidates, p.candidate(h, i+1))
	}

	outcome := recall.SearchSuccess
	if snap.coverage() != recall.IndexComplete {
		// Partial coverage is stated, never implied by a short list. A record
		// that failed to parse is unknown, not absent.
		outcome = recall.SearchPartial
	}
	diag := map[string]any{
		"query_mode": queryMode(terms, anyExact),
		"terms":      len(terms),
		"indexed":    len(snap.records),
		// matched counts before the limit was applied, so a caller comparing it
		// with the candidate list can see that a cap cut the tail.
		"matched":        matched,
		"coverage":       string(snap.coverage()),
		"failed_records": snap.failed,
		"generation":     snap.generation(),
		"elapsed_ms":     a.now().Sub(started).Milliseconds(),
	}
	if len(snap.missing) > 0 {
		diag["missing_files"] = snap.missing
	}
	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diag,
		SourceWatermark: snap.watermark(),
		Outcome:         outcome,
	}, nil
}

// hit is one record that survived filtering, with why it did.
type hit struct {
	rec   record
	score float64
	exact bool
}

// pass is the state one search shares with every candidate it renders. It
// exists so rendering does not reach back into the adapter and retake a lock
// per candidate.
type pass struct {
	snap     *snapshot
	set      Settings
	sourceID string
	floor    recall.Sensitivity
	termList []string
	variants recall.TermVariants
}

// candidate renders one record for fusion. The locator, the derivation edges,
// and the timestamps are the whole of what this source contributes; identity
// is stamped by the core.
func (p pass) candidate(h hit, rank int) recall.Candidate {
	rec := h.rec
	signals := []recall.MatchSignal{recall.MatchLexical}
	switch {
	case h.exact:
		signals = []recall.MatchSignal{recall.MatchExactIdentifier}
	case len(p.termList) == 0:
		// Nothing was matched textually: the window selected these records.
		signals = []recall.MatchSignal{recall.MatchField}
	}

	local := h.score
	rel := relevanceOf(rec, p.termList, p.variants)
	c := recall.Candidate{
		CandidateID:    rec.local(),
		SourceRecordID: rec.id,
		Locator:        recall.Locator{SourceID: p.sourceID, Local: rec.local()},
		DerivedFrom:    p.derivedFrom(rec),
		RecordType:     rec.kind,
		Title:          rec.title,
		Excerpt:        clip(rec.text, excerptBytes),
		LocalRank:      rank,
		LocalScore:     &local,
		Relevance:      &rel,
		MatchSignals:   signals,
		ObservedAt:     &rec.observedAt,
		ConfirmedAt:    &p.snap.builtAt,
		EventTime:      &rec.eventTime,
		SourceRevision: p.snap.generation(),
		Sensitivity:    p.floor,
		Metadata: map[string]any{
			"schema":      rec.schema,
			"file":        rec.file,
			"byte_offset": rec.offset,
		},
	}
	if rec.system != "" {
		c.Metadata["system"] = rec.system
	}
	if rec.ref != "" {
		c.Metadata["ref"] = rec.ref
	}
	if rec.actor != "" {
		c.Metadata["actor"] = rec.actor
	}
	if rec.revision != "" {
		c.Metadata["upstream_revision"] = rec.revision
	}
	return c
}

// derivedFrom names the upstream records this candidate projects.
//
// The edge is the configured source_id plus the upstream system's own
// identifier, which is exactly the locator that source writes for the same
// record — so the projection and the original collapse into one lineage root
// and never corroborate each other. A system with no configured mapping yields
// no edge: an invented source_id would resolve somewhere, and a wrong lineage
// root is worse than a missing one.
func (p pass) derivedFrom(rec record) []recall.Locator {
	if rec.system == "" || rec.ref == "" {
		return nil
	}
	sourceID, ok := p.set.Upstream[rec.system]
	if !ok {
		return nil
	}
	return []recall.Locator{{SourceID: sourceID, Local: rec.ref}}
}

// inWindow applies the request's time filters. as_of is honored here rather
// than refused because event_time is history the source already stores; see
// the package doc for why snapshot support would be a lie.
func inWindow(rec record, req recall.SearchRequest) bool {
	if req.AsOf != nil && rec.eventTime.After(*req.AsOf) {
		return false
	}
	if req.Filters.Since != nil && rec.eventTime.Before(*req.Filters.Since) {
		return false
	}
	if req.Filters.Until != nil && rec.eventTime.After(*req.Filters.Until) {
		return false
	}
	return true
}

func wantedType(rec record, want []recall.RecordType) bool {
	if len(want) == 0 {
		return true
	}
	for _, t := range want {
		if t == rec.kind {
			return true
		}
	}
	return false
}

// match scores a record against the query terms and reports whether any term
// matched a stable identifier at a token boundary. The score is the mean
// weight per term, so a long query is not scored above a precise one.
//
// A term is weighed under its own spelling or a discounted number variant of
// it, on the one definition every source shares: see [recall.WeighTerm].
func match(rec record, terms []string, variants recall.TermVariants) (score float64, exact bool) {
	if len(terms) == 0 {
		return 0, false
	}
	for _, term := range terms {
		exact = exact || rec.identifies(term)
		score += variants.Weigh(rec.weights, term)
	}
	return score / float64(len(terms)), exact
}

// relevanceOf is [recall.Candidate.Relevance] for one stream record.
func relevanceOf(rec record, terms []string, variants recall.TermVariants) float64 {
	return variants.RelevanceOverCounts(terms, rec.counts, rec.length)
}

// tokenize splits on anything that cannot appear inside an identifier.
//
// Identifier punctuation stays inside the token, so "td-f62256" is one token
// and can be compared for equality against a record's ref. A tokenizer that
// split on "-" would make exact identifier matching impossible for every
// system whose ids contain one, which is most of them.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		switch r {
		case '-', '_', '.', '/', ':':
			return false
		default:
			return true
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.Trim(f, "-_./:"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// clip bounds a preview at a rune boundary.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func queryMode(terms []string, exact bool) string {
	switch {
	case exact:
		return "exact"
	case len(terms) == 0:
		return "temporal"
	default:
		return "lexical"
	}
}

// stall delays a search and returns as soon as the context is done.
//
// This is the whole of what an adapter owes a cancellation: notice, return,
// and do not answer. The core's recall/cancel cancels the request context, and
// the server turns the returned context error into a protocol error rather
// than an empty success.
func stall(ctx context.Context, ms int) error {
	if ms <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
