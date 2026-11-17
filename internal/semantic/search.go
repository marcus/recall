package semantic

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Named numeric identifiers are checked in the original document or its path.
// Ordinary hyphenated query terms stay with similarity ranking.
var identifier = regexp.MustCompile(`(?i)\b[a-z][a-z0-9]*-[0-9]{2,}\b`)

func addAnchors(set map[string]struct{}, text string) {
	for _, anchor := range identifier.FindAllString(text, -1) {
		set[strings.ToLower(anchor)] = struct{}{}
	}
}

func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	if resp, skipped := adapter.UnsupportedFilters(req.Filters, "entities", "project"); skipped {
		return resp, nil
	}
	if req.Filters.Since != nil || req.Filters.Until != nil || req.AsOf != nil {
		return skipped(recall.SkipFilterUnsupported), nil
	}
	if types := req.Filters.RecordTypes; len(types) > 0 && !slices.Contains(types, recall.RecordDocument) {
		return skipped(recall.SkipRecordTypeMismatch), nil
	}
	if strings.TrimSpace(req.Query) == "" {
		return skipped(recall.SkipNotApplicable), nil
	}
	ctx, cancel := withDeadline(ctx, req.Deadline)
	defer cancel()

	fail := func(err error) (recall.SearchResponse, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return adapter.FailedSearch(err), err
	}

	vectors, err := a.embed.Embed(ctx, []string{req.Query}, true)
	if err != nil {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "query embedding: %v", err))
	}
	if len(vectors) != 1 {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "query embedding did not return one vector"))
	}
	current, err := a.scan(ctx)
	if err != nil {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "corpus boundary: %v", err))
	}
	lock, err := lockIndex(ctx, a.dir, "publish.lock", syscall.LOCK_SH)
	if err != nil {
		return fail(err)
	}
	defer lock.close()
	g, err := a.published()
	if err != nil {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "read Antfly generation: %v", err))
	}

	outcome := recall.SearchSuccess
	if current.Watermark != g.Watermark || g.Failed > 0 {
		outcome = recall.SearchPartial
	}
	response := recall.SearchResponse{
		Outcome:         outcome,
		Candidates:      []recall.Candidate{},
		SourceWatermark: g.Watermark,
		Diagnostics:     map[string]any{"index_generation": g.ID, "cosine_floor": a.floor},
	}
	if len(g.Chunks) == 0 {
		return response, nil
	}

	versions := make(map[string]string, len(current.Files))
	for _, file := range current.Files {
		versions[file.Path] = file.Version
	}
	index, err := a.open(filepath.Join(a.dir, g.ID, databaseFile))
	if err != nil {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "open Antfly index: %v", err))
	}
	defer func() { _ = index.Close() }()
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	anchors := identifier.FindAllString(strings.ToLower(req.Query), -1)
	candidates, err := a.rank(ctx, index, g, versions, anchors, vectors[0], limit, outcome == recall.SearchSuccess)
	if err != nil {
		return fail(protocol.Errorf(protocol.CodeSourceUnavailable, "exact Antfly lookup: %v", err))
	}
	response.Candidates = candidates
	return response, nil
}

func (a *Adapter) rank(ctx context.Context, index searchIndex, g *generation, versions map[string]string,
	anchors []string, vector []float32, limit int, complete bool) ([]recall.Candidate, error) {
	total := len(g.Chunks)
	limit = min(limit, total)
	fetch := min(total, max(64, limit))
	now := time.Now().UTC()
	for {
		hits, err := index.Search(ctx, vector, fetch)
		if err != nil {
			return nil, err
		}
		candidates := make([]recall.Candidate, 0, limit)
		seen := map[string]bool{}
		for _, hit := range hits {
			if hit.Score < a.floor {
				return candidates, nil
			}
			chunk, ok := g.Chunks[hit.ID]
			if !ok {
				return nil, fmt.Errorf("index returned a chunk absent from its generation metadata")
			}
			doc := g.Documents[chunk.Path]
			if seen[chunk.Path] || versions[chunk.Path] != doc.Version || !includesAnchors(doc.Anchors, anchors) {
				continue
			}
			seen[chunk.Path] = true
			local := fmt.Sprintf("%s#L%d-L%d", chunk.Path, chunk.Start, chunk.End)
			score := hit.Score
			relevance := math.Round(min(1, max(0, score))*10000) / 10000
			candidate := recall.Candidate{
				CandidateID:        local,
				SourceRecordID:     chunk.Path,
				Locator:            recall.Locator{SourceID: a.source, Local: local},
				RecordType:         recall.RecordDocument,
				Title:              doc.Title,
				Excerpt:            chunk.Excerpt,
				ExcerptKind:        recall.ExcerptMatched,
				LocalRank:          len(candidates) + 1,
				LocalScore:         &score,
				Relevance:          &relevance,
				MatchSignals:       []recall.MatchSignal{recall.MatchSemantic},
				ObservedAt:         &now,
				SourceRevision:     doc.Revision,
				Sensitivity:        recall.SensitivityInternal,
				ContentFingerprint: chunk.Fingerprint,
			}
			if complete {
				candidate.ConfirmedAt = &now
			}
			candidates = append(candidates, candidate)
			if len(candidates) == limit {
				return candidates, nil
			}
		}
		if fetch == total || len(hits) < fetch {
			return candidates, nil
		}
		fetch = min(total, fetch*2)
	}
}

func includesAnchors(fileAnchors, queryAnchors []string) bool {
	for _, anchor := range queryAnchors {
		if _, ok := slices.BinarySearch(fileAnchors, anchor); !ok {
			return false
		}
	}
	return true
}

func skipped(reason string) recall.SearchResponse {
	return recall.SearchResponse{
		Outcome: recall.SearchSkipped, Reason: reason, Candidates: []recall.Candidate{},
	}
}

func bounded(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
