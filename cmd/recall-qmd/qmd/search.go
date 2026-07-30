package qmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Search asks qmd for its ranked list and turns it into candidates.
//
// The division of labour is the point of this adapter. qmd owns retrieval and
// ordering: its rank becomes LocalRank, which is this source's whole
// contribution to how its candidates rank against each other. This process owns
// everything a caller has to be able to trust — whether the corpus searched is
// the one configured, whether the whole of it was seen, how much each result is
// actually ABOUT the query, and whether an empty list means "nothing matched" or
// "the models are not there".
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, sourceID, location, runner, err := a.session()
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	if resp, skipped := decline(req); skipped {
		return resp, nil
	}
	if req.AsOf != nil {
		// Declared as_of_support is none, so the core excludes this source from
		// a historical request before it gets here. Refusing again rather than
		// answering is what keeps that true if it ever does.
		err := protocol.Errorf(protocol.CodeAsOfUnsupported,
			"qmd: a qmd index describes current state and cannot answer an as_of query")
		return adapter.FailedSearch(err), err
	}
	if err := expired(ctx, req.Deadline); err != nil {
		return adapter.FailedSearch(err), err
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}

	// Probed per search, not read from a prior health result: `qmd collection
	// add` can re-point a collection at another directory at any time, and this
	// process is long-lived. A search that trusted an earlier probe would keep
	// answering from a corpus this source is no longer configured for.
	report, _, err := a.probeIndex(ctx, location)
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	coverage, why := coverageOf(report, set)
	if coverage == coverageNone {
		// This mode cannot see the corpus at all. An empty list here would be a
		// claim that the corpus was searched and holds nothing.
		err := protocol.Errorf(protocol.CodeSourceUnavailable, "qmd: %s", why)
		return adapter.FailedSearch(err), err
	}

	limit := set.MaxCandidates
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	args := searchArgs(set.Mode, set.Collection, limit, req.Query)
	res, err := a.run(ctx, args...)
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	hits, err := decodeResults(res, args...)
	if err != nil {
		return adapter.FailedSearch(err), err
	}

	watermark := sourceWatermark(report, set)
	terms := queryTerms(req.Query)
	candidates := make([]recall.Candidate, 0, len(hits))
	foreign, unreadable := 0, 0
	for _, hit := range hits {
		got, title, body, err := parseHit(hit, set.Collection)
		switch {
		case errors.Is(err, errForeignCollection):
			foreign++
			continue
		case err != nil:
			unreadable++
			continue
		}
		candidates = append(candidates, candidateOf(hit, got, title, body,
			sourceID, len(candidates)+1, set, terms, watermark))
	}

	reasons := make([]string, 0, 3)
	if why != "" {
		reasons = append(reasons, why)
	}
	if foreign > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d results named another collection and were dropped", foreign))
	}
	if unreadable > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d results could not be read as a document and a span", unreadable))
	}

	outcome := recall.SearchSuccess
	if coverage != coverageComplete || foreign > 0 || unreadable > 0 {
		// Every one of these means the returned list is a subset of what a
		// complete search would have produced, and nothing in the list itself
		// shows it.
		outcome = recall.SearchPartial
	}
	observed := a.now()
	for i := range candidates {
		candidates[i].ObservedAt = &observed
		if outcome == recall.SearchSuccess {
			// Only a complete boundary confirms. An incomplete scan observed
			// these records; it did not confirm the corpus they came from.
			confirmed := observed
			candidates[i].ConfirmedAt = &confirmed
		}
	}
	a.noteSuccess(observed)

	diag := map[string]any{
		"transport":  runner.Kind(),
		"mode":       string(set.Mode),
		"collection": set.Collection,
		"query_mode": string(queryModeOf(set.Mode)),
		// Which measure the relevance values in this response were produced on.
		// The two are not interchangeable and one source can be configured
		// either way, so a caller comparing two qmd instances is told rather
		// than left to infer it from the mode.
		"relevance_basis": relevanceBasis(set.Mode),
		"terms":           len(terms),
		"results":         len(hits),
		"index_documents": report.Documents,
		"index_vectors":   report.Vectors,
		"expansion":       set.Mode.Expands(),
		"rerank":          set.Mode.Reranks(),
		"elapsed_ms":      res.Elapsed.Milliseconds(),
	}
	if expanded := expandedQueries(hits, req.Query); len(expanded) > 0 {
		diag["expanded_queries"] = expanded
	}
	if len(reasons) > 0 {
		diag["coverage_reason"] = strings.Join(reasons, "; ")
	}
	if foreign > 0 {
		diag["foreign_collection_results"] = foreign
	}
	if unreadable > 0 {
		diag["unreadable_results"] = unreadable
	}

	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diag,
		SourceWatermark: watermark,
		Outcome:         outcome,
	}, nil
}

// decline answers the requests this source does not serve, before retrieval.
//
// Each of these has to be decided up front rather than by returning a broader
// result set as partial: candidates that do not satisfy the narrowed question
// are not evidence for it, and the core discards candidates attached to a
// skipped response precisely so a malformed adapter cannot leak them into
// fusion.
func decline(req recall.SearchRequest) (recall.SearchResponse, bool) {
	if resp, skipped := adapter.UnsupportedFilters(req.Filters, "entities", "project"); skipped {
		return resp, true
	}
	if req.Filters.Since != nil || req.Filters.Until != nil {
		// qmd has no time filter, and the only date available for a document is
		// its file mtime, which is a property of this checkout. Evaluating the
		// filter by guessing would invent matches; returning everything as
		// partial would let out-of-window documents answer a windowed question.
		names := make([]string, 0, 2)
		if req.Filters.Since != nil {
			names = append(names, "since")
		}
		if req.Filters.Until != nil {
			names = append(names, "until")
		}
		return recall.SearchResponse{
			Candidates: []recall.Candidate{},
			Outcome:    recall.SearchSkipped,
			Reason:     recall.SkipFilterUnsupported,
			Diagnostics: map[string]any{
				"unsupported_filters": names,
				"reason":              "a qmd index carries no record time this source may filter on",
			},
		}, true
	}
	if !wantsDocuments(req.Filters.RecordTypes) {
		return recall.SearchResponse{
			Candidates: []recall.Candidate{},
			Outcome:    recall.SearchSkipped,
			Reason:     recall.SkipRecordTypeMismatch,
			Diagnostics: map[string]any{
				"reason": "this source holds only documents",
			},
		}, true
	}
	if strings.TrimSpace(req.Query) == "" {
		// There is no browse boundary to fall back on: qmd's own grammar
		// refuses an empty query, and inventing one would make "everything this
		// instance happens to rank first" look like an answer to a question
		// nobody asked.
		return recall.SearchResponse{
			Candidates: []recall.Candidate{},
			Outcome:    recall.SearchSkipped,
			Reason:     recall.SkipNotApplicable,
			Diagnostics: map[string]any{
				"reason": "this source answers a query and has no browse boundary",
			},
		}, true
	}
	return recall.SearchResponse{}, false
}

func wantsDocuments(types []recall.RecordType) bool {
	if len(types) == 0 {
		return true
	}
	for _, kind := range types {
		if kind == recall.RecordDocument {
			return true
		}
	}
	return false
}

// queryModeOf names the retrieval a mode performed, for diagnostics.
func queryModeOf(mode Mode) recall.QueryMode {
	switch mode {
	case ModeBM25:
		return recall.QueryLexical
	default:
		return recall.QuerySemantic
	}
}

// expired reports a request that is already out of time.
//
// Both bounds are honored: the context, which the core cancels, and the deadline
// the request itself carries. Starting a qmd invocation that cannot finish would
// spend the caller's remaining budget to return nothing — and for a reranked
// query that budget is seconds.
func expired(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
