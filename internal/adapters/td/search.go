package td

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// excerptLimit bounds the evidence preview. A candidate is a pointer;
// docs/spec.md puts the payload behind Expand.
const excerptLimit = 240

// phraseProbeLimit is the longest query sent to td whole.
//
// td matches substrings, so a whole query only ever matches text containing it
// verbatim. That is worth one invocation for a short phrase — "vertical slice"
// finds the issue titled after it, and finds it with a stronger td score than
// either word alone — and worthless for a sentence, which cannot appear
// verbatim in a title. The cap is where a phrase stops being a phrase.
const phraseProbeLimit = 60

// searchScope states what td's search can and cannot see. It rides in every
// search's diagnostics because it is the difference between "no such issue"
// and "no issue whose title or description says that", and a caller reading a
// short result list has no other way to tell.
const searchScope = "td search matches id, title, and description only; text that exists only in a log, handoff, or comment is not searchable"

// Search returns this source's ranked candidates.
//
// Every failure path here returns a non-success outcome *and* an error. An
// adapter that returned `nil, nil` on a missing workspace would be
// indistinguishable from a workspace with no matching issues, and fusion
// downstream would believe it — invariant 2 in docs/spec.md.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, sourceID, floor, ws := a.config()
	if _, _, err := a.session(); err != nil {
		return fail(err, nil)
	}

	if req.AsOf != nil {
		// The manifest already declares AsOfNone, so the core should have
		// excluded this source. Refusing again is cheap and is the difference
		// between a bug upstream costing a wrong answer and costing a clear
		// error.
		return fail(fmt.Errorf("%w: td stores a last-write timestamp, not record history",
			protocol.ErrAsOfUnsupported), nil)
	}
	if len(req.Filters.RecordTypes) > 0 && !slices.Contains(req.Filters.RecordTypes, recall.RecordTask) {
		// Nothing failed: this source holds only issues and none were asked
		// for.
		return skipped("record_type"), nil
	}
	if req.Filters.Project != "" && !ws.answersTo(req.Filters.Project) {
		// A td workspace is a project, so a project filter is how a request
		// routes to one workspace among several. A workspace that is not the
		// one asked for has nothing to say — which is not the same as having
		// looked and found nothing, and is why this is a skip rather than a
		// search that returns empty.
		return skipped("project"), nil
	}

	started := a.now()
	terms := queryTerms(req.Query)
	probes := probeTerms(req.Query, terms, set.maxTermProbes())

	gathered := a.gather(ctx, set, probes)
	if err := gathered.fatal(len(probes)); err != nil {
		// Nothing that could answer this query did. Whether that is a missing
		// workspace or a wedged process, it is a failure and not an empty
		// workspace.
		return fail(err, gathered.timing())
	}

	exact, exactErr := a.resolveIdentifiers(ctx, identifierTokens(req.Query), gathered.byID)
	if exactErr != nil {
		return fail(exactErr, gathered.timing())
	}

	ranked := rank(gathered, exact, terms, set, req.Filters)
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
		candidates = append(candidates, a.candidate(s, i+1, sourceID, floor, ws, mark, started))
	}

	outcome := recall.SearchSuccess
	diagnostics := gathered.timing()
	diagnostics["workspace"] = ws.Name
	diagnostics["query_mode"] = queryMode(exact, probes)
	diagnostics["search_scope"] = searchScope
	diagnostics["probes"] = len(probes)
	diagnostics["corpus_records"] = len(gathered.records)
	diagnostics["matched"] = matched
	diagnostics["truncated"] = truncated

	switch {
	case gathered.corpusErr != nil:
		// The probes answered, so the search is real, but the listing that
		// confirms what the workspace holds did not. The result is missing its
		// freshness evidence and its structured fallback, which is a partial
		// answer rather than a whole one.
		outcome = recall.SearchPartial
		diagnostics["corpus"] = "unavailable"

	case len(gathered.records) >= corpusLimit:
		diagnostics["corpus_truncated"] = corpusLimit
	}
	if len(gathered.probeErrs) > 0 {
		// Some term probes failed and others did not, so recall is lower than
		// it should be. Saying partial is what stops a thin answer from being
		// read as a complete one.
		outcome = recall.SearchPartial
		diagnostics["failed_probes"] = len(gathered.probeErrs)
	}
	if gathered.probesTruncated > 0 {
		// The order below is decided by how many probes found each issue, and
		// a truncated probe means that count is a floor rather than a count: an
		// issue matching this term beyond the limit is indistinguishable here
		// from one that does not match it at all. Partial rather than success,
		// because the ranking is the thing that ran on incomplete input and a
		// caller reading a confident order has no other way to know.
		outcome = recall.SearchPartial
		diagnostics["probes_truncated"] = gathered.probesTruncated
		diagnostics["probe_limit"] = probeLimit
		diagnostics["term_coverage"] = "floor: a probe stopped at its limit, so an issue matching that term beyond it is not counted"
	}

	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diagnostics,
		SourceWatermark: mark,
		Outcome:         outcome,
	}, nil
}

// answersTo reports whether a project filter names this workspace. The match
// is case-insensitive because a project filter is something a person typed,
// and a workspace name is a directory name.
func (w workspace) answersTo(project string) bool {
	return strings.EqualFold(w.Name, project)
}

// skipped renders the one honest empty success: the source was not asked
// because it holds nothing of what was requested. It carries no error, which
// is what distinguishes it from every other empty result in this file.
func skipped(reason string) recall.SearchResponse {
	return recall.SearchResponse{
		Outcome:     recall.SearchSuccess,
		Diagnostics: map[string]any{"skipped": reason},
	}
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

func queryMode(exact []issue, probes []string) string {
	switch {
	case len(exact) > 0 && len(probes) > 0:
		return "exact+lexical"
	case len(exact) > 0:
		return "exact"
	case len(probes) > 0:
		return "lexical"
	default:
		return "structured"
	}
}

// probeTerms decides which text is sent to td, and in what order.
//
// The whole query goes first when it is short enough to be a phrase, because a
// verbatim hit is the strongest evidence td can produce. Terms follow, longest
// first: a long term is more selective, so it buys more recall per spawn than a
// short one. The cap is what keeps process count a function of configuration
// rather than of how much text a user pasted.
func probeTerms(query string, terms []string, limit int) []string {
	if limit <= 0 || len(terms) == 0 {
		return nil
	}
	byLength := slices.Clone(terms)
	slices.SortStableFunc(byLength, func(x, y string) int { return len(y) - len(x) })
	if len(byLength) > limit {
		byLength = byLength[:limit]
	}

	phrase := strings.TrimSpace(query)
	if len(terms) < 2 || len(phrase) > phraseProbeLimit {
		return byLength
	}
	return append([]string{phrase}, byLength...)
}

// gathered is everything the concurrent CLI phase produced.
type gathered struct {
	// records is the workspace listing: the query-independent read that
	// provides the watermark, the structured fallback, and free id lookups.
	records   []issue
	byID      map[string]issue
	corpusRaw []byte
	corpusErr error

	// hits maps issue id to what the probes found. One issue found by three
	// probes is one entry with three probes counted, not three candidates.
	hits      map[string]*hitState
	probeErrs []error

	// probesTruncated counts the probes that came back holding exactly their
	// limit, which means td had more matches to give. Coverage is counted over
	// what the probes returned, so each of these is a term whose match set was
	// only partly seen and a coverage number that is a floor rather than a
	// count. It is carried out of the gathering because a ranking judgment made
	// on partial input has to be reported as one.
	probesTruncated int

	invocations int
	wall        time.Duration
	process     time.Duration
}

// hitState is one issue as the probes saw it.
type hitState struct {
	rec issue

	// probes is how many distinct probes returned this issue. It is the one
	// ranking judgment this adapter adds: each td invocation knows only about
	// its own term, so only the merge can see that an issue answered to three
	// of the query's words and another answered to one.
	probes int

	// best is the highest score td gave this issue across probes, and hit is
	// the probe that produced it. td's score is a bucket, so keeping the best
	// one keeps td's own ordering intact inside a coverage tier.
	best int
	hit  searchHit

	fields map[string]struct{}
}

// fatal reports the error that makes this gathering unanswerable, or nil.
//
// A text query is answered by the probes, so losing every probe is fatal even
// when the listing succeeded: the listing is not a search, and returning it
// filtered by nothing would answer a question nobody asked. A structured
// query, which issues no probes, is answered by the listing alone, so losing
// it is fatal there. Losing some of either is degradation, reported as
// partial.
func (g gathered) fatal(probes int) error {
	if probes > 0 {
		if len(g.probeErrs) == probes {
			return g.probeErrs[0]
		}
		return nil
	}
	return g.corpusErr
}

func (g gathered) timing() map[string]any {
	return map[string]any{
		"cli_invocations": g.invocations,
		"cli_wall_ms":     g.wall.Milliseconds(),
		"cli_process_ms":  g.process.Milliseconds(),
	}
}

// gather runs every td invocation a search needs, concurrently.
//
// td is one-shot, so a search costs one spawn per invocation. Running them
// together makes the wall cost roughly one spawn while the reported process
// time stays the honest total, which is the number to watch if this design
// ever needs replacing with an index.
func (a *Adapter) gather(ctx context.Context, set settings, probes []string) gathered {
	start := time.Now()
	out := gathered{hits: make(map[string]*hitState)}

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
		args := listArgs(set)
		res, err := a.run(ctx, args...)
		record(res)
		var records []issue
		if err == nil {
			err = decodeJSON(res, &records, args...)
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out.corpusErr = err
			return
		}
		byID := make(map[string]issue, len(records))
		for _, rec := range records {
			if rec.valid() {
				byID[rec.ID] = rec
			}
		}
		out.records, out.byID, out.corpusRaw = records, byID, res.Stdout
	}()

	for _, probe := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := searchArgs(set, probe, probeLimit)
			res, err := a.run(ctx, args...)
			record(res)
			var hits []searchHit
			if err == nil {
				err = decodeJSON(res, &hits, args...)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.probeErrs = append(out.probeErrs, err)
				return
			}
			if len(hits) >= probeLimit {
				// td stopped at the limit rather than at the end of its match
				// set, so this term's contribution to every issue's coverage
				// count was computed over part of what matched. Counted before
				// the records are filtered, because the question is what td
				// returned and not what survived parsing.
				out.probesTruncated++
			}
			for _, hit := range hits {
				if !hit.Issue.valid() {
					continue
				}
				out.merge(hit)
			}
		}()
	}

	wg.Wait()
	out.wall = time.Since(start)
	return out
}

// merge folds one probe's hit into the pool. The caller holds the lock.
func (g *gathered) merge(hit searchHit) {
	state, seen := g.hits[hit.Issue.ID]
	if !seen {
		state = &hitState{rec: hit.Issue, fields: map[string]struct{}{}}
		g.hits[hit.Issue.ID] = state
	}
	state.probes++
	if hit.Score > state.best || !seen {
		state.best, state.hit = hit.Score, hit
	}
	if hit.MatchField != "" {
		state.fields[hit.MatchField] = struct{}{}
	}
}

// resolveIdentifiers turns id-shaped query tokens into records.
//
// The listing answers most lookups for free. A token the listing does not hold
// still gets one `td show` probe, because an exact id lookup must reach an
// issue outside the configured scope: asking about a specific id is asking
// about that issue, not about the open list.
//
// td normalizes a bare `369eef` into `td-369eef` and matches case-insensitively,
// so the returned record is evidence of what td resolved, never proof that the
// query named that identifier. The id is compared again here.
func (a *Adapter) resolveIdentifiers(ctx context.Context, tokens []string, byID map[string]issue) ([]issue, error) {
	var out []issue
	for _, token := range tokens {
		if rec, ok := byID[token]; ok {
			out = append(out, rec)
			continue
		}
		rec, err := a.fetchIssue(ctx, token)
		switch {
		case errors.Is(err, errNotFound):
			// The workspace answered and holds no such issue. That is a fact
			// about this source, not a failure of it.
			continue
		case err != nil:
			return nil, err
		case rec.ID == token && rec.valid():
			out = append(out, rec)
		}
	}
	return out, nil
}

// fetchIssue reads one issue by id, with everything `td show` carries: the
// description, the acceptance criteria, the progress log, the latest handoff,
// and the recent review decisions.
func (a *Adapter) fetchIssue(ctx context.Context, id string) (issue, error) {
	args := []string{"show", id, "--json"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return issue{}, err
	}
	var rec issue
	if err := decodeJSON(res, &rec, args...); err != nil {
		return issue{}, err
	}
	return rec, nil
}

// scored is one candidate with the evidence that ranked it.
type scored struct {
	rec     issue
	state   *hitState
	exact   bool
	order   int
	signals []recall.MatchSignal

	// confirmed records that the workspace listing — a complete enumeration of
	// this instance's scope — held this issue. A record known only from a
	// search probe was observed, not confirmed present by a boundary.
	confirmed bool
}

// rank applies the request's filters and orders what survives.
//
// The order is: exact id hits, then how many probes found the issue, then td's
// own score, then priority, then most recently updated, then id. Only the
// second of those is this adapter's judgment; the third is td's, and the rest
// are tie-breaks chosen to be deterministic, so the same workspace and the same
// query always produce the same order.
func rank(g gathered, exact []issue, terms []string, set settings, filters recall.Filters) []scored {
	exactOrder := make(map[string]int, len(exact))
	for i, rec := range exact {
		exactOrder[rec.ID] = i
	}

	pool := make([]scored, 0, len(g.hits)+len(exact))
	add := func(s scored) {
		if !s.rec.valid() {
			return
		}
		if !s.exact && !set.keeps(s.rec) {
			return
		}
		if !keepsRequest(s.rec, filters) {
			return
		}
		pool = append(pool, s)
	}

	for _, rec := range exact {
		// An exact id is a direct instruction to retrieve one record, so it
		// bypasses the instance's own scoping. Filters that came with the
		// request still apply: those are what the caller asked for now.
		_, confirmed := g.byID[rec.ID]
		add(scored{
			rec: rec, state: g.hits[rec.ID], exact: true, order: exactOrder[rec.ID],
			signals: signalsFor(g.hits[rec.ID], true), confirmed: confirmed,
		})
	}
	for id, state := range g.hits {
		if _, already := exactOrder[id]; already {
			continue
		}
		_, confirmed := g.byID[id]
		add(scored{
			rec: state.rec, state: state,
			signals: signalsFor(state, false), confirmed: confirmed,
		})
	}
	if len(terms) == 0 {
		// A query with no probeable terms is a structured request: the filters
		// are the question. The listing is the answer, and every record in it
		// already passed the instance's scope and td's own filters.
		for _, rec := range g.records {
			if _, already := exactOrder[rec.ID]; already {
				continue
			}
			add(scored{rec: rec, signals: []recall.MatchSignal{recall.MatchField}, confirmed: true})
		}
	}

	slices.SortStableFunc(pool, compare)
	return pool
}

// signalsFor explains why a candidate surfaced.
//
// A td `id` match is a field match, not an identifier match: td scores any
// issue whose id merely CONTAINS the query text, and docs/adapter-protocol.md
// reserves exact_identifier for a token-boundary match on a whole identifier.
// That signal is set here only for a token this adapter itself recognized and
// then confirmed against the workspace.
func signalsFor(state *hitState, exact bool) []recall.MatchSignal {
	var signals []recall.MatchSignal
	if exact {
		signals = append(signals, recall.MatchExactIdentifier)
	}
	if state == nil {
		return signals
	}
	var lexical, field bool
	for f := range state.fields {
		switch f {
		case "title", "description":
			lexical = true
		default:
			field = true
		}
	}
	if lexical {
		signals = append(signals, recall.MatchLexical)
	}
	if field {
		signals = append(signals, recall.MatchField)
	}
	return signals
}

// compare is the total order candidates are emitted in.
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
	if c := y.coverage() - x.coverage(); c != 0 {
		return c
	}
	if c := y.tdScore() - x.tdScore(); c != 0 {
		return c
	}
	if p := x.rec.priorityOrder() - y.rec.priorityOrder(); p != 0 {
		return p
	}
	// Most recently touched first: among issues that match a query equally
	// well, the one someone worked on last week is the one being asked about.
	if c := y.rec.UpdatedAt.Compare(x.rec.UpdatedAt); c != 0 {
		return c
	}
	return strings.Compare(x.rec.ID, y.rec.ID)
}

func (s scored) coverage() int {
	if s.state == nil {
		return 0
	}
	return s.state.probes
}

func (s scored) tdScore() int {
	if s.state == nil {
		return 0
	}
	return s.state.best
}

// keepsRequest applies the filters that came with this request, as opposed to
// the instance's configured scoping.
func keepsRequest(rec issue, filters recall.Filters) bool {
	if len(filters.Entities) > 0 && !mentionsEntity(rec, filters.Entities) {
		return false
	}
	if filters.Since != nil || filters.Until != nil {
		// The window is applied to when the issue was raised, which is the
		// event this record is about. Using updated_at would answer "what was
		// touched then" with today's content, which is the same overclaim the
		// manifest refuses for as_of.
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

// mentionsEntity matches a requested entity against the handles a td issue
// actually has: its labels, its epic, and the sessions that worked on it.
func mentionsEntity(rec issue, entities []string) bool {
	handles := rec.handles()
	for _, entity := range entities {
		if containsFold(handles, entity) {
			return true
		}
	}
	return false
}

// candidate builds the envelope for one ranked issue.
func (a *Adapter) candidate(
	s scored,
	localRank int,
	sourceID string,
	floor recall.Sensitivity,
	ws workspace,
	mark string,
	observed time.Time,
) recall.Candidate {
	var hit *searchHit
	var score *float64
	if s.state != nil {
		hit = &s.state.hit
		native := float64(s.state.best)
		score = &native
	}

	c := recall.Candidate{
		// The id is stable within the workspace and unique in a result list,
		// so it serves as the candidate identity too. One issue never produces
		// two candidates here: there is no chunking, so source_record_id and
		// candidate_id coincide by construction rather than by coincidence.
		// The workspace is not part of source_record_id because the source
		// instance already is the workspace, and corroboration collapses on
		// source_uid plus this value.
		CandidateID:    s.rec.ID,
		SourceRecordID: s.rec.ID,
		Locator:        recall.Locator{SourceID: sourceID, Local: ws.locator(s.rec.ID)},
		RecordType:     recall.RecordTask,
		Title:          s.rec.Title,
		Excerpt:        excerpt(s.rec),
		LocalRank:      localRank,
		LocalScore:     score,
		MatchSignals:   s.signals,

		ObservedAt: &observed,
		EventTime:  s.rec.eventTime(),
		ValidFrom:  s.rec.validFrom(),
		ValidTo:    s.rec.validTo(),

		SourceRevision: mark,
		Sensitivity:    floor,
		Metadata:       s.rec.metadata(ws, hit),

		// Two instances can resolve to ONE td database — a repository and a
		// subdirectory of it name the same database, because td walks upward to
		// find one. Lineage groups on source_uid plus source_record_id, so
		// without this those instances hand the core one issue twice under two
		// identities and it is counted as two independent pieces of evidence,
		// collecting the corroboration bonus for agreeing with itself. One
		// issue, read once, out of one database, is one observation. The
		// fingerprint says so, and record_type plus content_fingerprint
		// collapses them without the core needing to know anything about td.
		ContentFingerprint: fingerprint(s.rec.ID, s.rec.UpdatedAt),
	}
	if s.confirmed {
		// The listing enumerated this instance's whole scope, so a record it
		// held is confirmed present and not merely observed. A record only a
		// search probe returned gets no confirmation, because a search is not
		// a boundary.
		c.ConfirmedAt = &observed
	}
	return c
}

// fingerprint identifies the observation: which issue, and which version of it.
//
// Everything that varies between two instances reading one database is
// deliberately excluded. Not the workspace name, not the configured location,
// not the resolved root — those are precisely what the two instances disagree
// about, so a fingerprint built on any of them would differ for the same issue
// and defeat its own purpose. They would also make the value depend on where a
// checkout lives, which would put a machine's directory layout into every
// recorded transcript.
//
// The issue's own updated_at is the version, in preference to the workspace
// watermark. A watermark fingerprints the whole listing, so two instances
// scoped to different statuses read different listings and produce different
// watermarks for issues they both returned; updated_at is a property of the
// record, so it is the same value in every scope that can see the record.
//
// Two different databases collide only by holding the same six-hex id updated
// at the same nanosecond. td mints ids randomly per database, so that is not a
// case worth trading the above for.
func fingerprint(issueID string, updated time.Time) string {
	sum := sha256.Sum256([]byte(issueID + "\x00" + updated.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])
}

// excerpt is the bounded preview: the rendered headline plus the opening of
// the description, which is where a td issue says what it is about.
func excerpt(rec issue) string {
	text := rec.headline()
	if body := firstParagraph(rec.Description); body != "" {
		text += " — " + body
	}
	return truncate(text, excerptLimit)
}

// firstParagraph is the description up to its first blank line, flattened. td
// descriptions are Markdown with headings and lists; the first paragraph is
// the part written as prose.
func firstParagraph(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if cut := strings.Index(body, "\n\n"); cut >= 0 {
		body = body[:cut]
	}
	return strings.Join(strings.Fields(body), " ")
}

// truncate cuts text to a byte limit at the last whitespace inside it, falling
// back to the last rune boundary. Cutting mid-rune would emit invalid UTF-8,
// which is a worse failure than a slightly shorter excerpt.
//
// The limit counts the ellipsis, because docs/adapter-protocol.md calls
// budget_bytes a hard output limit: a caller that sized a budget and got three
// bytes more would have no bound at all.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…"
	room := limit - len(ellipsis)
	if room <= 0 {
		// Not enough budget to say anything and mark it cut. Returning the
		// mark alone would be a byte count with no content in it.
		return ""
	}
	cut := room
	for cut > 0 && !isSpace(s[cut]) {
		cut--
	}
	if cut == 0 {
		cut = room
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
	}
	return strings.TrimRight(s[:cut], " \n\t") + ellipsis
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\t' }
