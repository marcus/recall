package qmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// A view reuses the existing qmd contract, parsers and coverage rules for one
// collection. It carries no mirror, lifecycle state, or independent transport.
func (a *Adapter) collectionView(derived bool) (*Adapter, error) {
	set, id, location, runner, err := a.session()
	if err != nil {
		return nil, err
	}
	if derived {
		set.Collection = set.DerivedCollection
		location = a.paths().Mirror
	}
	set.DeriveExtensions = nil
	return &Adapter{opts: Options{Clock: a.now}, ready: true, sourceID: id, location: location, runner: runner, settings: set}, nil
}

var errDerivedMissing = fmt.Errorf("%w: derived collection missing; run recall refresh", protocol.ErrSourceUnavailable)

// Missing is distinguished from malformed, denied, or unreachable. qmd 2.5.3
// exits 1 for a missing collection; some recordings use exit 0. Only the exact
// named absence message permits creation, and only during refresh.
func (a *Adapter) ensureDerived(ctx context.Context, create bool) error {
	if err := a.validateMirror(); err != nil {
		return err
	}
	view, err := a.collectionView(true)
	if err != nil {
		return err
	}
	res, err := a.run(ctx, "collection", "show", view.settings.Collection)
	if err != nil {
		return err
	}
	missing := false
	want := "Collection not found: " + view.settings.Collection
	if res.ExitCode == 0 || res.ExitCode == 1 {
		missing = scrub(string(res.Stdout)) == want || scrub(string(res.Stderr)) == want
	}
	if !missing {
		text, err := decodeText(res, "collection", "show")
		if err != nil {
			return err
		}
		report, err := parseCollection(text, view.settings.Collection)
		if err != nil {
			return err
		}
		if err := checkCollectionPath(report, view.location); err != nil {
			return fmt.Errorf("%w; if upgrading a v0.7.0 profile-owned mirror, choose an unused derived_collection name and run recall refresh --source <source_id>; see docs/qmd-adapter.md#upgrading-from-v070", err)
		}
		return nil
	}
	if !create {
		return errDerivedMissing
	}
	if err = os.MkdirAll(view.location, 0o700); err != nil {
		return err
	}
	res, err = a.runMaintenance(ctx, "collection", "add", view.location, "--name", view.settings.Collection, "--mask", "**/*.md")
	if err != nil {
		return err
	}
	if _, err = decodeText(res, "collection", "add"); err != nil {
		return err
	}
	_, err = view.verifyCollection(ctx, view.location)
	return err
}

func (a *Adapter) healthDerived(ctx context.Context) (recall.Health, error) {
	primary, err := a.collectionView(false)
	if err != nil {
		return a.unhealthy(err), err
	}
	health, err := primary.Health(ctx)
	if err != nil {
		return health, err
	}
	view, err := a.collectionView(true)
	if err != nil {
		return health, err
	}
	derived, err := view.Health(ctx)
	if err != nil {
		return health, err
	}
	r, scan, why := a.inspectMirror(ctx)
	if ctx.Err() != nil {
		return recall.Health{}, ctx.Err()
	}
	if err = a.validateMirror(); err != nil {
		why = safeErr(err)
	}
	health.Diagnostics["derived_collection"] = view.settings.Collection
	health.Diagnostics["derived_health"] = derived.Status
	health.Diagnostics["mirrored_files"] = len(r.Entries)
	health.Diagnostics["excluded_files"] = scan.Excluded
	health.Diagnostics["excluded_dirs"] = scan.ExcludedDirs
	if scan.Entries == nil {
		health.Diagnostics["excluded_files"] = r.Excluded
		health.Diagnostics["excluded_dirs"] = r.ExcludedDirs
	}
	health.Diagnostics["derived_indexed_count"] = derived.IndexedCount
	health.Diagnostics["derived_index_watermark"] = derived.IndexWatermark
	health.Diagnostics["derived_failures"] = scan.Failures
	if r.Ready && derived.IndexedCount != int64(len(r.Entries)) {
		why = joinReason(why, "derived index count does not match mirror")
	}
	if derived.Status != recall.HealthHealthy {
		why = joinReason(why, "derived collection: "+string(derived.Status))
		health.Diagnostics["derived_detail"] = derived.Diagnostics
	}
	if why != "" {
		health.Diagnostics["derived_reason"] = why
		health.Diagnostics["derived_health"] = recall.HealthDegraded
	}
	// A primary failure does not erase a usable semantic view, and vice versa.
	if health.Status != recall.HealthHealthy || why != "" {
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexPartial
	} else {
		health.RecordCount += int64(len(r.Entries))
		health.IndexedCount += derived.IndexedCount
	}
	health.IndexConfig += " " + a.derivedDigest()
	health.SourceWatermark += " derived=" + receiptDigest(r) + " current=" + scanDigest(scan)
	health.IndexWatermark += " derived=" + derived.IndexWatermark
	health.IndexGeneration = "qmd-gen-" + hashBytes([]byte(health.IndexGeneration + receiptDigest(r)))[:12]
	health.FailedCount += int64(len(scan.Failures))
	if health.Status == recall.HealthHealthy {
		a.noteSuccess(health.CheckedAt)
	}
	health.LastSuccess = a.lastSuccessful()
	return health, nil
}
func receiptDigest(r mirrorReceipt) string { // entries are serialized with sorted keys by encoding/json
	data, _ := json.Marshal(r)
	return hashBytes(data)
}
func (a *Adapter) derivedDigest() string { set, _, _, _, _ := a.session(); return set.settingsDigest() }
func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func (a *Adapter) refreshDerived(ctx context.Context, p protocol.RefreshParams) (recall.Health, error) {
	set, _, root, _, err := a.session()
	if err != nil {
		return recall.Health{}, err
	}
	if !p.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, p.Deadline)
		defer cancel()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, set.RefreshTimeout)
		defer cancel()
	}
	if ctx.Err() != nil {
		return recall.Health{}, ctx.Err()
	}
	ctx, release, err := a.lockDerived(ctx, true)
	if err != nil {
		if ctx.Err() == nil {
			return a.unhealthy(err), nil
		}
		return recall.Health{}, err
	}
	defer release()
	primary, err := a.collectionView(false)
	if err != nil {
		return recall.Health{}, err
	}
	// Refuse a wrong path before any artifact or index mutation.
	if _, err = primary.verifyCollection(ctx, root); err == nil {
		err = a.ensureDerived(ctx, false)
		if errors.Is(err, errDerivedMissing) {
			err = nil // absence permits creation only after durable invalidation
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return recall.Health{}, ctx.Err()
		}
		h := a.unhealthy(err)
		h.Diagnostics["derived_reason"] = safeErr(err)
		return h, nil
	}
	r := mirrorReceipt{Version: 1, Corpus: canonical(root), Config: set.settingsDigest(), Failure: "refresh did not complete"}
	// Durable invalidation precedes all mirror and index changes. A crash, failed
	// embed, or cancellation cannot leave a previous success receipt in force.
	if err = a.saveReceipt(r); err != nil {
		return a.unhealthy(err), nil
	}
	// Collection add, update, and embed all write qmd's index, which other
	// sources may share, so they run under the index-wide maintenance lock
	// (#16). It is taken after the derived lock, never before it.
	ctx, releaseIndex, err := a.lockIndex(ctx, a.resolveIndexPath(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return recall.Health{}, ctx.Err()
		}
		r.Failure = safeErr(err)
		_ = a.saveReceipt(r)
		return a.unhealthy(err), nil
	}
	defer releaseIndex()
	// Collection add can itself index files, so it belongs after invalidation.
	// Recheck its path under the same lock immediately before maintenance.
	err = a.ensureDerived(ctx, true)
	var scan mirrorScan
	if err == nil {
		scan, err = a.scanMirror(ctx)
	}
	if err == nil {
		err = a.syncMirror(ctx, scan)
		r.Entries = scan.Entries
		r.Excluded = scan.Excluded
		r.ExcludedDirs = scan.ExcludedDirs
	}
	if err == nil {
		steps := primary.refreshSteps(set)
		steps[0].args = []string{"update", "-c", set.Collection}
		text := func(res Result) (any, error) { return decodeText(res, "derived maintenance") }
		steps = append(steps, refreshStep{name: "derived update", args: []string{"update", "-c", set.DerivedCollection}, check: text}, refreshStep{name: "derived embed", args: []string{"embed", "-c", set.DerivedCollection}, check: text})
		for _, step := range steps {
			if err = a.ensureDerived(ctx, false); err != nil {
				break
			}
			if _, err = primary.verifyCollection(ctx, root); err != nil {
				break
			}
			var res Result
			res, err = a.runMaintenance(ctx, step.args...)
			if err == nil {
				_, err = step.check(res)
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", step.name, err)
				break
			}
		}
	}
	if err == nil && len(scan.Failures) > 0 {
		err = fmt.Errorf("%d derived files could not be extracted: %s", len(scan.Failures), strings.Join(scan.Failures, "; "))
	}
	r.Ready = err == nil
	r.Failure = safeErr(err)
	if saveErr := a.saveReceipt(r); saveErr != nil {
		return a.unhealthy(saveErr), nil
	}
	if ctx.Err() != nil {
		return recall.Health{}, ctx.Err()
	}
	h, healthErr := a.healthDerived(ctx)
	if err != nil {
		h.Diagnostics["last_refresh_error"] = safeErr(err)
		h.Status = recall.HealthDegraded
		h.Coverage = recall.IndexPartial
	}
	return h, healthErr
}

func (a *Adapter) searchDerived(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	if resp, skip := decline(req); skip {
		return resp, nil
	}
	if req.AsOf != nil {
		err := protocol.Errorf(protocol.CodeAsOfUnsupported, "qmd: a qmd index cannot answer an as_of query")
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
	ctx, release, err := a.lockDerived(ctx, false)
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	defer release()
	primary, err := a.collectionView(false)
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	response, primaryErr := primary.Search(ctx, req)
	if ctx.Err() != nil {
		return adapter.FailedSearch(ctx.Err()), ctx.Err()
	}
	if response.Diagnostics == nil {
		response.Diagnostics = map[string]any{}
	}
	response.Diagnostics["primary_outcome"] = response.Outcome
	reason := ""
	if primaryErr != nil {
		reason = "primary collection: " + safeErr(primaryErr)
	}
	r, scan, why := a.inspectMirror(ctx)
	var derived recall.SearchResponse
	var derivedErr error
	if derivedErr = a.ensureDerived(ctx, false); derivedErr == nil && why == "" {
		view, e := a.collectionView(true)
		if e != nil {
			return adapter.FailedSearch(e), e
		}
		report, _, probeErr := view.probeIndex(ctx, view.location)
		derivedErr = probeErr
		if probeErr == nil && (!report.Collection.HasFiles || report.Collection.Files != len(scan.Entries)) {
			derivedErr = fmt.Errorf("%w: derived index does not match mirrored file count", protocol.ErrSourceUnavailable)
		}
		if derivedErr == nil {
			derived, derivedErr = view.Search(ctx, req)
		}
		if derivedErr == nil {
			// Model retrieval may take seconds. Recheck the current bytes and durable
			// receipt after it returns, including edits/noindex/deletions during that
			// await. A span is mapped only against the exact refresh generation.
			current, currentScan, currentWhy := a.inspectMirror(ctx)
			if currentWhy != "" || receiptDigest(current) != receiptDigest(r) {
				why = joinReason(why, "derived corpus changed during query; run recall refresh")
			} else {
				scan = currentScan
			}
			for _, candidate := range derived.Candidates {
				if why != "" {
					break
				}
				mapped, e := a.mapDerived(candidate, scan)
				if e != nil {
					why = joinReason(why, "derived result could not be mapped to current original text: "+safeErr(e))
					continue
				}
				response.Candidates = append(response.Candidates, mapped)
			}
		}
	}
	if ctx.Err() != nil {
		return adapter.FailedSearch(ctx.Err()), ctx.Err()
	}
	if derivedErr != nil {
		why = joinReason(why, safeErr(derivedErr))
	}
	if derived.Outcome == recall.SearchPartial {
		why = joinReason(why, "derived query coverage is partial")
	}
	reason = joinReason(reason, why)
	if primaryErr != nil && (derivedErr != nil || why != "") && len(response.Candidates) == 0 {
		err = fmt.Errorf("%w: %s", protocol.ErrSourceUnavailable, reason)
		return adapter.FailedSearch(err), err
	}
	if response.Outcome != recall.SearchSuccess || reason != "" {
		response.Outcome = recall.SearchPartial
		response.Diagnostics["coverage_reason"] = joinReason(stringDiagnostic(response.Diagnostics["coverage_reason"]), reason)
	} else {
		response.Outcome = recall.SearchSuccess
	}
	// Rank positions from separate collection searches are comparable only as
	// ranks. Interleave them deterministically; never compare RRF/cosine scores
	// against one another or let one collection consume the other's entire pool.
	sort.SliceStable(response.Candidates, func(i, j int) bool { return response.Candidates[i].LocalRank < response.Candidates[j].LocalRank })
	set, _, _, _, _ := a.session()
	limit := set.MaxCandidates
	if req.Limit > 0 {
		limit = min(limit, req.Limit)
	}
	if len(response.Candidates) > limit {
		response.Candidates = response.Candidates[:limit]
	}
	for i := range response.Candidates {
		response.Candidates[i].LocalRank = i + 1
		if response.Outcome != recall.SearchSuccess {
			response.Candidates[i].ConfirmedAt = nil
		}
	}
	response.Diagnostics["derived_collection"] = set.DerivedCollection
	response.Diagnostics["derived_outcome"] = derived.Outcome
	response.Diagnostics["mirrored_files"] = len(r.Entries)
	response.Diagnostics["excluded_files"] = scan.Excluded
	response.SourceWatermark += " derived=" + receiptDigest(r) + " current=" + scanDigest(scan)
	if response.Outcome == recall.SearchSuccess {
		a.noteSuccess(a.now())
	}
	return response, nil
}

// Mirror offsets select extracted sections; only original source ranges ever
// leave this adapter. Synthetic title lines map to the document, not line 1 of
// a mirror masquerading as line 1 of HTML.
func (a *Adapter) mapDerived(c recall.Candidate, scan mirrorScan) (recall.Candidate, error) {
	ref, err := parseLocal(c.Locator.Local)
	if err != nil {
		return c, err
	}
	if !strings.HasSuffix(ref.Path, ".md") {
		return c, fmt.Errorf("derived hit has no mirror suffix")
	}
	rel := ""
	for original, entry := range scan.Entries {
		if entry.QMDPath == ref.Path {
			if rel != "" {
				return c, fmt.Errorf("ambiguous derived virtual path")
			}
			rel = original
		}
	}
	doc, ok := scan.Docs[rel]
	if !ok {
		return c, fmt.Errorf("derived original not admitted")
	}
	line := 3 // # title, then one blank line
	var chunks []document.Chunk
	for _, chunk := range doc.HTML.Chunks {
		text := chunk.Text()
		if text == "" {
			continue
		}
		end := line + strings.Count(text, "\n")
		if ref.Start <= end && ref.End >= line {
			chunks = append(chunks, chunk)
		}
		line = end + 3
	}
	if ref.Start >= line {
		return c, fmt.Errorf("derived hit outside rendered text")
	}
	original := span{Path: rel, Start: 1, End: doc.HTML.SourceLines, Stated: true}
	title := doc.HTML.Title
	body := doc.HTML.Markdown()
	if len(chunks) > 0 {
		original.Start = chunks[0].StartLine
		original.End = chunks[len(chunks)-1].EndLine
		var parts []string
		for _, chunk := range chunks {
			parts = append(parts, chunk.Text())
		}
		body = strings.Join(parts, "\n\n")
		title = htmlChunkTitle(doc.HTML.Title, chunks[0])
	}
	c.SourceRecordID = rel
	c.Locator.Local = original.Local()
	c.CandidateID = rel + "@" + original.Local()
	c.Title = sanitizeLine(title)
	c.Excerpt = clip(sanitizeBlock(body), excerptBytes)
	c.SourceRevision = fileRevision(doc.Data)
	c.Metadata["path"] = rel
	c.Metadata["start_line"] = original.Start
	c.Metadata["end_line"] = original.End
	for _, key := range []string{"anchor_line", "context_before", "context_after", "span_inferred"} {
		delete(c.Metadata, key)
	}
	if len(chunks) > 0 {
		c.Metadata["heading_path"] = chunks[0].HeadingPath
		if chunks[0].Anchor != "" {
			c.Metadata["anchor"] = chunks[0].Anchor
		}
	}
	return c, nil
}
func htmlChunkTitle(title string, c document.Chunk) string {
	p := c.HeadingPath
	if len(p) > 0 && p[0] == title {
		p = p[1:]
	}
	if len(p) == 0 {
		return title
	}
	return title + " > " + strings.Join(p, " > ")
}

func (a *Adapter) expandDerived(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	set, _, root, _, err := a.session()
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if err = expired(ctx, req.Deadline); err != nil {
		return recall.ExpandResponse{}, err
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	ctx, release, err := a.lockDerived(ctx, false)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	defer release()
	ref, err := parseLocal(req.Locator.Local)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	primary, err := a.collectionView(false)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if _, err = primary.verifyCollection(ctx, root); err != nil {
		return recall.ExpandResponse{}, err
	}
	if err = a.ensureDerived(ctx, false); err != nil {
		return recall.ExpandResponse{}, err
	}
	if !derives(set, ref.Path) {
		return primary.Expand(ctx, req)
	}
	// Expand can serve current original text even when embeddings are stale, but
	// it must reapply the complete current boundary, including sibling preference
	// and noindex. It never reads the mirror as evidence.
	scan, err := a.scanMirror(ctx)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	doc, ok := scan.Docs[ref.Path]
	if !ok {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "qmd: %s is gone, excluded, or cannot be extracted", ref.Path)
	}
	if ref.Start > doc.HTML.SourceLines {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "qmd: original source range no longer exists")
	}
	if req.Budget > 0 && req.Budget < minimumBudget {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeBudgetExceeded, "qmd: budget cannot carry evidence")
	}
	served, content, boundary := renderDerivedHTML(doc.HTML, ref, req.Detail)
	if boundary != "" {
		content += truncationMarker(boundary)
	}
	if req.Budget > 0 && int64(len(content)) > req.Budget {
		content = cutToBudget(content, req.Budget)
		boundary = boundaryBudget
	}
	return recall.ExpandResponse{Content: sanitizeBlock(content), SourceRevision: fileRevision(doc.Data), Provenance: served.String() + " in collection " + set.DerivedCollection, Truncated: boundary != "", TruncationBoundary: boundary}, nil
}
func renderDerivedHTML(d document.HTML, ref chunkRef, detail recall.DetailLevel) (chunkRef, string, string) {
	var lines []string
	var ranges []chunkRef
	start, end := -1, 0
	for _, c := range d.Chunks {
		from := len(lines)
		for _, line := range strings.Split(c.Text(), "\n") {
			lines = append(lines, line)
			ranges = append(ranges, chunkRef{Path: ref.Path, Start: c.StartLine, End: c.EndLine})
		}
		if c.EndLine >= ref.Start && c.StartLine <= ref.End {
			if start < 0 {
				start = from
			}
			end = len(lines)
		}
	}
	if start < 0 {
		return ref, "", ""
	}
	served := ref
	served.End = min(ref.End, d.SourceLines)
	boundary := ""
	if served.End < ref.End {
		boundary = boundaryFileEnd
	}
	if detail == recall.DetailContext {
		start = max(0, start-contextLines)
		end = min(len(lines), end+contextLines)
	}
	if detail != recall.DetailFull && detail != recall.DetailContext && end-start > excerptLines {
		end = start + excerptLines
		boundary = boundaryExcerpt
	}
	served.Start = ranges[start].Start
	served.End = ranges[end-1].End
	content := strings.Join(lines[start:end], "\n")
	if detail == recall.DetailSummary {
		content = d.Title + "\n" + content
		if len([]rune(content)) > summaryRunes {
			content = string([]rune(content)[:summaryRunes])
			boundary = boundarySummary
		}
	}
	return served, content, boundary
}

func stringDiagnostic(v any) string { s, _ := v.(string); return s }

func scanDigest(s mirrorScan) string { data, _ := json.Marshal(s.Entries); return hashBytes(data) }
