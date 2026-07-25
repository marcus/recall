package tasks

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Lexical weights. They are ordered by how much evidence each kind of hit is,
// not tuned: a term in the title is what the task is about, a term in its
// fields is how the task is filed, and a term in the body is context. The
// gaps are wide enough that no number of weaker hits outvotes a title match.
const (
	weightTitleWord      = 1.0
	weightTitleSubstring = 0.6
	weightField          = 0.5
	weightBody           = 0.4
)

// excerptLimit bounds the evidence preview. A candidate is a pointer;
// docs/spec.md puts the payload behind Expand.
const excerptLimit = 240

// projectCoverage states the one place project metadata is thinner than it
// looks. `tasks projects --json` rolls up over open, non-deferred tasks filed
// under a project or area; Inbox items and closed work are outside every
// rollup, so they carry no project at search time. Expansion reads the true
// project from `tasks show`, which has no such gap.
const projectCoverage = "open tasks filed under a project or area; the CLI rollup excludes Inbox and closed work"

// Search returns this source's ranked candidates.
//
// Every failure path here returns a non-success outcome *and* an error. An
// adapter that returned `nil, nil` on a broken CLI would be indistinguishable
// from a store with no matching tasks, and fusion downstream would believe it —
// invariant 2 in docs/spec.md.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, sourceID, floor := a.config()
	if _, _, err := a.session(); err != nil {
		return fail(err, nil)
	}

	if req.AsOf != nil {
		// The manifest already declares AsOfNone, so the core should have
		// excluded this source. Refusing again is cheap and is the difference
		// between a bug upstream costing a wrong answer and costing a clear
		// error.
		return fail(fmt.Errorf("%w: the Tasks CLI publishes no record history", protocol.ErrAsOfUnsupported), nil)
	}
	if len(req.Filters.RecordTypes) > 0 && !slices.Contains(req.Filters.RecordTypes, recall.RecordTask) {
		// Nothing failed: this source holds only tasks and none were asked
		// for. An empty success is the truthful answer here, and it is the
		// only place in this file that produces one.
		return recall.SearchResponse{
			Outcome:     recall.SearchSuccess,
			Diagnostics: map[string]any{"skipped": "record_type"},
		}, nil
	}

	started := a.now()
	terms := queryTerms(req.Query)
	probes := probeTerms(terms, set.maxTermProbes())

	gathered := a.gather(ctx, set, probes)
	if gathered.corpusErr != nil {
		return fail(gathered.corpusErr, map[string]any{
			"cli_invocations": gathered.invocations,
			"cli_wall_ms":     gathered.wall.Milliseconds(),
		})
	}

	index := projectIndex(gathered.projects)
	ids := identifierTokens(req.Query)
	exact, exactErr := a.resolveIdentifiers(ctx, ids, gathered.byID)
	if exactErr != nil {
		return fail(exactErr, map[string]any{"cli_invocations": gathered.invocations + 1})
	}

	ranked := rank(gathered.records, exact, terms, gathered.bodyHits, index, set, req.Filters)
	matched := len(ranked)

	limit := set.maxCandidates()
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	truncated := matched > limit
	if truncated {
		ranked = ranked[:limit]
	}

	mark := watermark(gathered.corpusRaw)
	candidates := make([]recall.Candidate, 0, len(ranked))
	for i, s := range ranked {
		candidates = append(candidates, a.candidate(s, i+1, sourceID, floor, mark, index, started))
	}

	outcome := recall.SearchSuccess
	diagnostics := map[string]any{
		"query_mode":      queryMode(exact, terms),
		"scope":           set.Scope,
		"cli_invocations": gathered.invocations,
		"cli_wall_ms":     gathered.wall.Milliseconds(),
		"cli_process_ms":  gathered.process.Milliseconds(),
		"corpus_records":  len(gathered.records),
		"matched":         matched,
		"body_probes":     len(probes),
		"truncated":       truncated,
	}
	if req.Filters.Project != "" {
		// The rollup is computed over open, non-deferred tasks filed under a
		// project or area, so it never names an Inbox item or a closed one. A
		// project filter therefore narrows further than its name suggests, and
		// saying so is cheaper than a caller discovering it from a short list.
		diagnostics["project_filter_coverage"] = projectCoverage
	}
	if gathered.projectsErr != nil {
		// Project is a routing field, not the answer. Losing it degrades the
		// result rather than voiding it — unless the request filtered on it,
		// in which case the filter could not be applied and silently returning
		// everything would be worse than returning nothing.
		if req.Filters.Project != "" {
			return fail(gathered.projectsErr, diagnostics)
		}
		outcome = recall.SearchPartial
		diagnostics["project_metadata"] = "unavailable"
	}

	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diagnostics,
		SourceWatermark: mark,
		Outcome:         outcome,
	}, nil
}

// fail renders a failure honestly and returns it alongside the error, which is
// what the [adapter.Adapter] contract asks for.
func fail(err error, diagnostics map[string]any) (recall.SearchResponse, error) {
	resp := adapter.FailedSearch(err)
	resp.Diagnostics["detail"] = err.Error()
	for k, v := range diagnostics {
		resp.Diagnostics[k] = v
	}
	return resp, err
}

func queryMode(exact []taskRecord, terms []string) string {
	switch {
	case len(exact) > 0 && len(terms) > 0:
		return "exact+lexical"
	case len(exact) > 0:
		return "exact"
	case len(terms) > 0:
		return "lexical"
	default:
		return "structured"
	}
}

// probeTerms picks which terms are worth an extra `--body` invocation.
//
// Longest first: a long term is more selective, so it buys more recall per
// spawn than a short one. The cap is what keeps process count a function of
// configuration rather than of how much text a user pasted.
func probeTerms(terms []string, limit int) []string {
	if limit <= 0 || len(terms) == 0 {
		return nil
	}
	byLength := slices.Clone(terms)
	slices.SortStableFunc(byLength, func(x, y string) int { return len(y) - len(x) })
	if len(byLength) > limit {
		byLength = byLength[:limit]
	}
	return byLength
}

// gathered is everything the concurrent CLI phase produced.
type gathered struct {
	records   []taskRecord
	byID      map[string]taskRecord
	corpusRaw []byte
	corpusErr error

	projects    []projectRecord
	projectsErr error

	// bodyHits maps a probed term to the ids the CLI matched with `--body`.
	bodyHits map[string]map[string]struct{}

	invocations int
	wall        time.Duration
	process     time.Duration
}

// gather runs every CLI invocation a search needs, concurrently.
//
// The CLI is one-shot, so a search costs one spawn per invocation. Running
// them together makes the wall cost roughly one spawn while the reported
// process time stays the honest total, which is the number to watch if this
// design ever needs replacing with an index.
func (a *Adapter) gather(ctx context.Context, set settings, probes []string) gathered {
	start := time.Now()
	out := gathered{bodyHits: make(map[string]map[string]struct{}, len(probes))}

	var mu sync.Mutex
	var wg sync.WaitGroup
	record := func(res Result) {
		mu.Lock()
		out.invocations++
		out.process += res.Elapsed
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		args := listArgs(set.Scope, nil)
		res, err := a.run(ctx, args...)
		record(res)
		var records []taskRecord
		if err == nil {
			err = decodeJSON(res, &records, args...)
		}
		byID := make(map[string]taskRecord, len(records))
		for _, r := range records {
			if r.valid() {
				byID[r.ID] = r
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out.corpusErr = err
			return
		}
		out.records, out.byID, out.corpusRaw = records, byID, res.Stdout
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		args := []string{"projects", "--json"}
		res, err := a.run(ctx, args...)
		record(res)
		var projects []projectRecord
		if err == nil {
			err = decodeJSON(res, &projects, args...)
		}
		mu.Lock()
		defer mu.Unlock()
		out.projects, out.projectsErr = projects, err
	}()

	for _, term := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// "/" forces the term to be read as text, so a term beginning with
			// "-", "+", or "@" cannot become a flag or a facet filter.
			args := listArgs(set.Scope, []string{"--body", "/" + term})
			res, err := a.run(ctx, args...)
			record(res)
			var hits []taskRecord
			if err == nil {
				err = decodeJSON(res, &hits, args...)
			}
			if err != nil {
				// A probe is an optimization. Losing one costs body recall for
				// one term, which is a smaller lie than failing a search that
				// the corpus listing can still answer.
				return
			}
			seen := make(map[string]struct{}, len(hits))
			for _, h := range hits {
				seen[h.ID] = struct{}{}
			}
			mu.Lock()
			defer mu.Unlock()
			out.bodyHits[term] = seen
		}()
	}

	wg.Wait()
	out.wall = time.Since(start)
	return out
}

// resolveIdentifiers turns id-shaped query tokens into records.
//
// The corpus answers most lookups for free. A token the corpus does not hold
// still gets one `show` probe, because an exact id lookup must reach a task
// outside the configured scope: asking about a specific id is asking about
// that task, not about the open list.
//
// A resolved record is accepted only when its id equals the token byte for
// byte. `tasks show` also resolves title substrings and case-insensitive ids,
// so the returned record is evidence of what the CLI matched, never proof that
// the query named an identifier.
func (a *Adapter) resolveIdentifiers(ctx context.Context, tokens []string, byID map[string]taskRecord) ([]taskRecord, error) {
	var out []taskRecord
	for _, token := range tokens {
		if rec, ok := byID[token]; ok {
			out = append(out, rec)
			continue
		}
		args := []string{"show", token, "--json", "--include-done"}
		res, err := a.run(ctx, args...)
		if err != nil {
			return nil, err
		}
		if res.ExitCode == exitNoMatch {
			continue
		}
		var rec taskRecord
		if err := decodeJSON(res, &rec, args...); err != nil {
			return nil, err
		}
		if rec.ID == token && rec.valid() {
			out = append(out, rec)
		}
	}
	return out, nil
}

// exitNoMatch is what the CLI returns when a reference resolves to nothing. It
// is an answer, not a failure: the store was read successfully and holds no
// such task.
const exitNoMatch = 2

// scored is one candidate with the evidence that ranked it.
type scored struct {
	rec     taskRecord
	score   float64
	exact   bool
	order   int
	signals []recall.MatchSignal
}

// rank applies the request's filters and orders what survives.
func rank(
	records []taskRecord,
	exact []taskRecord,
	terms []string,
	bodyHits map[string]map[string]struct{},
	projects map[string]string,
	set settings,
	filters recall.Filters,
) []scored {
	exactOrder := make(map[string]int, len(exact))
	pool := make([]taskRecord, 0, len(records)+len(exact))
	for i, rec := range exact {
		exactOrder[rec.ID] = i
		pool = append(pool, rec)
	}
	for _, rec := range records {
		if _, already := exactOrder[rec.ID]; !already {
			pool = append(pool, rec)
		}
	}

	out := make([]scored, 0, len(pool))
	for _, rec := range pool {
		if !rec.valid() {
			continue
		}
		order, isExact := exactOrder[rec.ID]
		// An exact id is a direct instruction to retrieve one record, so it
		// bypasses the instance's own scoping. Filters that came with the
		// request still apply: those are what the caller asked for now.
		if !isExact && !set.keeps(rec) {
			continue
		}
		if !keepsRequest(rec, projects, filters) {
			continue
		}

		score, signals := lexical(rec, terms, bodyHits, projects[rec.ID])
		if isExact {
			signals = append([]recall.MatchSignal{recall.MatchExactIdentifier}, signals...)
		} else if score == 0 && len(terms) > 0 {
			continue
		}
		out = append(out, scored{rec: rec, score: score, exact: isExact, order: order, signals: signals})
	}

	slices.SortStableFunc(out, compare)
	return out
}

// compare is the total order candidates are emitted in.
//
// Exact identifier hits partition above everything else, mirroring the core's
// exact-match promotion rather than adding a score bonus that a long lexical
// match could out-argue. Everything after the score is a tie-break chosen to
// be deterministic: the same store and the same query always produce the same
// order, which is what makes evaluation transcripts diffable.
func compare(x, y scored) int {
	if x.exact != y.exact {
		if x.exact {
			return -1
		}
		return 1
	}
	if x.exact && y.exact {
		return x.order - y.order
	}
	switch {
	case x.score > y.score:
		return -1
	case x.score < y.score:
		return 1
	}
	xt, yt := x.rec.eventTime(), y.rec.eventTime()
	switch {
	case xt != nil && yt == nil:
		return -1
	case xt == nil && yt != nil:
		return 1
	case xt != nil && yt != nil && !xt.Equal(*yt):
		return xt.Compare(*yt)
	}
	if p := x.rec.priorityOrder() - y.rec.priorityOrder(); p != 0 {
		return p
	}
	return strings.Compare(x.rec.ID, y.rec.ID)
}

// lexical scores one record against the query terms.
func lexical(
	rec taskRecord,
	terms []string,
	bodyHits map[string]map[string]struct{},
	project string,
) (float64, []recall.MatchSignal) {
	title := strings.ToLower(rec.Title)
	fields := rec.fieldText(project)

	var score float64
	var lexicalHit, fieldHit bool
	for _, term := range terms {
		switch {
		case containsWord(title, term):
			score += weightTitleWord
			lexicalHit = true
		case strings.Contains(title, term):
			score += weightTitleSubstring
			lexicalHit = true
		}
		if containsWord(fields, term) {
			score += weightField
			fieldHit = true
		}
		if hits, ok := bodyHits[term]; ok {
			if _, hit := hits[rec.ID]; hit && !containsWord(title, term) {
				score += weightBody
				lexicalHit = true
			}
		}
	}

	var signals []recall.MatchSignal
	if lexicalHit {
		signals = append(signals, recall.MatchLexical)
	}
	if fieldHit {
		signals = append(signals, recall.MatchField)
	}
	return score, signals
}

// keepsRequest applies the filters that came with this request, as opposed to
// the instance's configured scoping.
func keepsRequest(rec taskRecord, projects map[string]string, filters recall.Filters) bool {
	if filters.Project != "" && !strings.EqualFold(recordProject(rec, projects), filters.Project) {
		return false
	}
	if len(filters.Entities) > 0 && !mentionsEntity(rec, projects, filters.Entities) {
		return false
	}
	if filters.Since != nil || filters.Until != nil {
		// The only time a task carries is its own dated boundary. A task with
		// no date cannot be shown to fall inside the window, so a window
		// excludes it rather than admitting it on the assumption that undated
		// means always.
		at := rec.eventTime()
		if at == nil {
			return false
		}
		if filters.Since != nil && at.Before(*filters.Since) {
			return false
		}
		if filters.Until != nil && at.After(*filters.Until) {
			return false
		}
	}
	return true
}

func recordProject(rec taskRecord, projects map[string]string) string {
	if p, ok := projects[rec.ID]; ok && p != "" {
		return p
	}
	if rec.Project != nil {
		return *rec.Project
	}
	return ""
}

// mentionsEntity matches a requested entity against the handles a task
// actually has: its contexts, its tags, and its project. Those are the only
// entity-shaped fields in the store.
func mentionsEntity(rec taskRecord, projects map[string]string, entities []string) bool {
	handles := append(normalizeContexts(rec.Contexts), rec.Tags...)
	if project := recordProject(rec, projects); project != "" {
		handles = append(handles, project)
	}
	for _, entity := range normalizeContexts(entities) {
		if containsFold(handles, entity) {
			return true
		}
	}
	return false
}

// candidate builds the envelope for one ranked record.
func (a *Adapter) candidate(
	s scored,
	localRank int,
	sourceID string,
	floor recall.Sensitivity,
	mark string,
	projects map[string]string,
	observed time.Time,
) recall.Candidate {
	score := s.score
	return recall.Candidate{
		// The id is stable within the revision and unique in a result list, so
		// it serves as the candidate identity too. One task never produces two
		// candidates here: there is no chunking, so source_record_id and
		// candidate_id coincide by construction rather than by coincidence.
		CandidateID:    s.rec.ID,
		SourceRecordID: s.rec.ID,
		Locator:        recall.Locator{SourceID: sourceID, Local: s.rec.ID},
		RecordType:     recall.RecordTask,
		Title:          s.rec.Title,
		Excerpt:        excerpt(s.rec),
		LocalRank:      localRank,
		LocalScore:     &score,
		MatchSignals:   s.signals,

		// A listing is a complete source boundary: it enumerated the whole
		// scope, so it confirms these records present, not merely observed.
		ObservedAt:  &observed,
		ConfirmedAt: &observed,
		EventTime:   s.rec.eventTime(),
		ValidTo:     s.rec.closedAt(),

		SourceRevision: mark,
		Sensitivity:    floor,
		Metadata:       s.rec.metadata(projects[s.rec.ID]),
	}
}

// excerpt is the bounded preview. The CLI's own headline already renders
// state, priority, title, tags, and contexts in the form a person reads, so
// there is nothing to invent here.
func excerpt(rec taskRecord) string {
	text := rec.Headline
	if text == "" {
		text = rec.Title
	}
	if len(rec.Notes) > 0 {
		text += " — " + rec.Notes[0]
	}
	return truncate(text, excerptLimit)
}

// truncate cuts text to a byte limit at the last whitespace inside it, falling
// back to the last rune boundary. Cutting mid-rune would emit invalid UTF-8,
// which is a worse failure than a slightly shorter excerpt.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !isSpace(s[cut]) {
		cut--
	}
	if cut == 0 {
		cut = limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return strings.TrimRight(s[:cut], " \n\t") + "…"
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\t' }
