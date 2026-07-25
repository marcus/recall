package claracorpus

import (
	"context"
	"sort"
	"strings"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/recall"
)

// Search returns this source's own ranked candidates.
//
// Ranking is deliberately simple and explainable:
//
//  1. Exact identifier hits first — a query token equal to a record id, a
//     signal's ref, an upstream native id, or a locator's local part. This is a
//     partition, not a bonus, mirroring the core's own exact-match promotion.
//  2. Then by standing: live records above inactive ones above archived ones.
//     This is Clara's verdict carried through, never an age this adapter
//     computed, and it is a partition because "still current" is a different
//     claim from "matched slightly better".
//  3. Then by term coverage, multiplied for memory by Clara's effective weight.
//     A faded record is demoted by a bounded factor and never removed.
//  4. Ties broken by newest event first, then by id, so the order is total and
//     reproducible.
//
// A query with no terms is a browse, which is a real question to ask either
// store: for signals it is the newest events, and for memory it is what
// currently carries the most weight — which is Clara's own recall order.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	s, err := a.session()
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	if resp, skipped := adapter.UnsupportedFilters(req.Filters, "entities", "project"); skipped {
		return resp, nil
	}
	if err := stall(ctx, s.set.StallMS); err != nil {
		return adapter.FailedSearch(err), err
	}

	started := a.now()
	snap, err := a.current(false)
	if err != nil {
		// An unreadable store is never a search that succeeded with no matches.
		// The error carries the code; the response carries the outcome for a
		// caller holding both.
		return adapter.FailedSearch(err), err
	}

	terms := tokenize(req.Query)
	// A request that names a time window is asking a historical question, and
	// docs/spec.md#decay says such a question retrieves old evidence without a
	// recency penalty. The records were already selected by the window;
	// demoting them for being old would answer a different question.
	windowed := req.Filters.Since != nil || req.Filters.Until != nil

	var (
		hits     []hit
		anyExact bool
	)
	for i := range snap.items {
		if err := ctx.Err(); err != nil {
			return adapter.FailedSearch(err), err
		}
		it := &snap.items[i]
		if !inWindow(it, req) || !wantedType(it, req.Filters.RecordTypes) {
			continue
		}
		score, exact := match(it, terms, windowed)
		if len(terms) > 0 && score == 0 && !exact {
			continue
		}
		anyExact = anyExact || exact
		hits = append(hits, hit{item: it, score: score, exact: exact})
	}
	matched := len(hits)

	sort.SliceStable(hits, func(i, j int) bool {
		l, r := hits[i], hits[j]
		switch {
		case l.exact != r.exact:
			return l.exact
		case l.item.standing != r.item.standing:
			return l.item.standing < r.item.standing
		case l.score != r.score:
			return l.score > r.score
		case !l.item.eventTime.Equal(r.item.eventTime):
			return l.item.eventTime.After(r.item.eventTime)
		default:
			return l.item.id < r.item.id
		}
	})

	limit := s.set.maxCandidates()
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	candidates := make([]recall.Candidate, 0, len(hits))
	for i, h := range hits {
		candidates = append(candidates, candidateOf(h, i+1, snap, s, len(terms)))
	}

	outcome := recall.SearchSuccess
	if snap.coverage() != recall.IndexComplete {
		// Partial coverage is stated, never implied by a short list. A record
		// that failed to parse is unknown, not absent, and a signal whose
		// action state could not be read is a signal missing evidence this
		// source claims to carry.
		outcome = recall.SearchPartial
	}
	diag := map[string]any{
		"store":      string(s.store),
		"query_mode": queryMode(terms, anyExact),
		"terms":      len(terms),
		"indexed":    len(snap.items),
		// matched counts before the limit was applied, so a caller comparing it
		// with the candidate list can see that a cap cut the tail.
		"matched":    matched,
		"coverage":   string(snap.coverage()),
		"generation": snap.generation(),
		"elapsed_ms": a.now().Sub(started).Milliseconds(),
	}
	if snap.failed > 0 {
		diag["failed_records"] = snap.failed
	}
	if snap.obsFailed > 0 {
		// Named separately from failed_records: the signals are all here, and
		// what is unknown is what the owner did about some of them.
		diag["failed_observation_records"] = snap.obsFailed
	}
	if snap.duplicates > 0 {
		diag["duplicate_records_resolved"] = snap.duplicates
	}
	if len(snap.absent) > 0 {
		diag["absent_files"] = snap.absent
	}
	if windowed {
		diag["decay_applied"] = false
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
	item  *item
	score float64
	exact bool
}

// candidateOf renders one item for fusion. The locator, the derivation edges,
// and the timestamps are the whole of what this source contributes; identity is
// stamped by the core.
func candidateOf(h hit, rank int, snap *snapshot, s session, terms int) recall.Candidate {
	it := h.item
	signals := []recall.MatchSignal{recall.MatchLexical}
	switch {
	case h.exact:
		signals = []recall.MatchSignal{recall.MatchExactIdentifier}
	case terms == 0:
		// Nothing was matched textually: standing and weight selected these.
		signals = []recall.MatchSignal{recall.MatchField}
	}

	local := h.score
	meta := make(map[string]any, len(it.metadata))
	for k, v := range it.metadata {
		meta[k] = v
	}

	c := recall.Candidate{
		CandidateID:    it.local,
		SourceRecordID: it.id,
		Locator:        recall.Locator{SourceID: s.sourceID, Local: it.local},
		DerivedFrom:    it.derived,
		RecordType:     it.recordType,
		Title:          it.title,
		Excerpt:        it.excerpt,
		LocalRank:      rank,
		LocalScore:     &local,
		MatchSignals:   signals,
		// Recall observed this record when the generation was built.
		ObservedAt:         &snap.builtAt,
		SourceRevision:     snap.generation(),
		Sensitivity:        it.sensitivity,
		Metadata:           meta,
		ContentFingerprint: it.fingerprint,
	}
	if snap.coverage() == recall.IndexComplete {
		// Confirmation means a complete scan established the record against the
		// whole declared store. A partial generation observed this candidate but
		// cannot honestly confirm it.
		c.ConfirmedAt = &snap.builtAt
	}
	if !it.eventTime.IsZero() {
		event := it.eventTime
		c.EventTime = &event
	}
	c.ValidFrom, c.ValidTo = it.validFrom, it.validTo
	return c
}

// inWindow applies the request's time filters against the record's event time.
//
// as_of never arrives: the manifest declares [recall.AsOfNone], so the core
// excludes this source from a request carrying a historical boundary. These
// filters are scope on a question about the present, which is a different thing
// and one these records can answer.
func inWindow(it *item, req recall.SearchRequest) bool {
	if it.eventTime.IsZero() {
		// A record with no event time cannot be excluded by one. Dropping it
		// would turn "this store does not date that record" into "the record is
		// outside your window", which is a false absence.
		return true
	}
	if req.Filters.Since != nil && it.eventTime.Before(*req.Filters.Since) {
		return false
	}
	if req.Filters.Until != nil && it.eventTime.After(*req.Filters.Until) {
		return false
	}
	return true
}

func wantedType(it *item, want []recall.RecordType) bool {
	if len(want) == 0 {
		return true
	}
	for _, t := range want {
		if t == it.recordType {
			return true
		}
	}
	return false
}

// match scores a record against the query terms and reports whether any term
// matched a stable identifier at a token boundary.
//
// The text score is the mean weight per term, so a long query is not scored
// above a precise one. For memory it is then multiplied by the decay factor,
// which is where Clara's effective weight enters ranking — and only ranking:
// fusion consumes local rank, so a decayed weight never reaches a cross-source
// comparison.
func match(it *item, terms []string, windowed bool) (score float64, exact bool) {
	if len(terms) == 0 {
		// A browse. Memory answers by what currently carries weight, signals by
		// recency, which the sort already applies.
		if it.decays && !windowed {
			return it.dec.Effective, false
		}
		return 0, false
	}
	for _, term := range terms {
		exact = exact || it.identifies(term)
		score += it.weights[term]
	}
	score /= float64(len(terms))
	if it.decays && !windowed {
		score *= it.dec.multiplier()
	}
	return score, exact
}

// identifies reports whether term is one of this record's stable identifiers,
// compared whole. An unbounded substring match never counts.
func (it *item) identifies(term string) bool {
	for _, id := range it.identifiers {
		if id != "" && strings.EqualFold(id, term) {
			return true
		}
	}
	return false
}

func queryMode(terms []string, exact bool) string {
	switch {
	case exact:
		return "exact"
	case len(terms) == 0:
		return "structured"
	default:
		return "lexical"
	}
}
