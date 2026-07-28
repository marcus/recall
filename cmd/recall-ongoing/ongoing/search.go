package ongoing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// excerptBytes bounds a candidate's preview. A candidate is a pointer, not a
// payload; the locator is how a caller gets the rest. It is generous by this
// repository's standards because the preview is where the attention reasons
// land, and "dormant" without them is the noise this source exists to replace.
const excerptBytes = 320

// Field weights. A match on the project's name says more about the record than
// a match on a commit subject, and the ordering here is the whole of what
// "more relevant" means inside this source.
const (
	weightName      = 1.0
	weightPath      = 0.9
	weightFullPath  = 0.7
	weightDecision  = 0.6 // note, next action: the owner's own words
	weightAttribute = 0.5 // language, intent, view keys, repository handles
	weightNarrative = 0.4 // attention reason text, latest commit subject
)

// Search returns this source's own ranked candidates.
//
// Ranking is deliberately simple and explainable:
//
//  1. Exact identifier hits first — a query token equal to a project's id,
//     name, or path relative to its scan root. This is a partition, not a
//     bonus, mirroring the core's own exact-match promotion.
//  2. Everything else by term coverage over the fields weighted above.
//  3. Ties broken by most recent commit first, then by name, then by id, so
//     the order is total and reproducible.
//
// The tiebreak is ongoing's own default dashboard sort, not a judgement this
// adapter invented. Nothing here combines the attention classifications into a
// rank: ongoing deliberately computes no priority score, and a source that
// quietly added one would be answering a different question from the system it
// reports on.
//
// A query with no terms is a browse, which is a real thing to ask a project
// catalog — "what am I working on" — and it returns the catalog in that same
// order, narrowed by whatever filters came with the request.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	set, tr, sourceID, floor, err := a.session()
	if err != nil {
		return adapter.FailedSearch(err), err
	}
	if err := stall(ctx, set.StallMS); err != nil {
		return adapter.FailedSearch(err), err
	}
	if req.AsOf != nil {
		// The manifest declares AsOfNone, so the core should have excluded this
		// source already. Answering anyway would answer a historical question
		// from current state, which is the one thing as_of support exists to
		// prevent, so the boundary is refused here too.
		err := fmt.Errorf("%w: the catalog keeps no history of a project's fields",
			protocol.ErrAsOfUnsupported)
		return adapter.FailedSearch(err), err
	}

	started := a.now()
	cat, err := a.fetch(ctx, tr, set)
	if err != nil {
		// An unreachable or refused instance is never a search that succeeded
		// with no matches. The error carries the code; the response carries the
		// outcome for a caller holding both.
		return adapter.FailedSearch(err), err
	}
	if err := ctx.Err(); err != nil {
		return adapter.FailedSearch(err), err
	}

	terms := tokenize(req.Query)
	// Resolved once per search over the whole catalog, never per project: see
	// [recall.ResolveTermVariants] for why the gate is source-wide.
	variants := recall.ResolveTermVariants(terms, cat.holds)
	var (
		hits     []hit
		anyExact bool
	)
	for i := range cat.Projects {
		p := &cat.Projects[i]
		if !set.keeps(p) || !keepsRequest(p, req.Filters) {
			continue
		}
		score, exact := match(p, terms, variants)
		if len(terms) > 0 && score == 0 && !exact {
			continue
		}
		anyExact = anyExact || exact
		hits = append(hits, hit{project: p, score: score, exact: exact})
	}
	matched := len(hits)

	sort.SliceStable(hits, func(i, j int) bool {
		l, r := hits[i], hits[j]
		switch {
		case l.exact != r.exact:
			return l.exact
		case l.score != r.score:
			return l.score > r.score
		default:
			return newerCommit(l.project, r.project)
		}
	})

	limit := set.maxCandidates()
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	observed := a.now()
	p := pass{cat: cat, sourceID: sourceID, floor: floor, termList: terms, variants: variants, observed: observed}
	candidates := make([]recall.Candidate, 0, len(hits))
	for i, h := range hits {
		candidates = append(candidates, p.candidate(h, i+1))
	}

	outcome := recall.SearchSuccess
	if cat.coverage() != recall.IndexComplete {
		// Partial coverage is stated, never implied by a short list. A scan
		// that has not finished may not yet have written a project that
		// exists, and a record missing from a partial pass is unknown rather
		// than absent.
		outcome = recall.SearchPartial
	}
	diag := map[string]any{
		"query_mode": queryMode(terms, anyExact, req.Filters),
		"terms":      len(terms),
		"projects":   len(cat.Projects),
		// matched counts before the limit was applied, so a caller comparing it
		// with the candidate list can see that a cap cut the tail.
		"matched":    matched,
		"coverage":   string(cat.coverage()),
		"revision":   cat.revision(),
		"transport":  tr.kind(),
		"elapsed_ms": a.now().Sub(started).Milliseconds(),
	}
	if len(set.Views) > 0 {
		diag["views_filter"] = set.Views
	}
	if cat.stale() {
		// A stale catalog still answers, and it answers completely. Saying so
		// here is what stops a caller reading a three-day-old attention state
		// as today's.
		age, _ := cat.age()
		diag["catalog_age_hours"] = int(age.Hours())
		diag["stale"] = true
	}
	for k, v := range cat.staleCollectors() {
		// Per-collector staleness travels with the search, not only with
		// health, because it is the search result that gets read. A project
		// absent from an attention view because its LOC measurement expired is
		// indistinguishable in the candidate list from one that genuinely does
		// not qualify.
		diag[k] = v
	}
	return recall.SearchResponse{
		Candidates:      candidates,
		Diagnostics:     diag,
		SourceWatermark: cat.watermark(),
		Outcome:         outcome,
	}, nil
}

// hit is one project that survived filtering, with why it did.
type hit struct {
	project *project
	score   float64
	exact   bool
}

// pass is the state one search shares with every candidate it renders. It
// exists so rendering does not reach back into the adapter and retake a lock
// per candidate.
type pass struct {
	cat      *catalog
	sourceID string
	floor    recall.Sensitivity
	termList []string
	variants recall.TermVariants
	observed time.Time
}

// candidate renders one project for fusion.
func (p pass) candidate(h hit, rank int) recall.Candidate {
	proj := h.project
	signals := []recall.MatchSignal{recall.MatchLexical}
	switch {
	case h.exact:
		signals = []recall.MatchSignal{recall.MatchExactIdentifier}
	case len(p.termList) == 0:
		// Nothing was matched textually: the filters, or the absence of any,
		// selected these projects.
		signals = []recall.MatchSignal{recall.MatchField}
	}

	local := h.score
	rel := relevance(proj, p.termList, p.variants)
	c := recall.Candidate{
		CandidateID:    proj.ID,
		SourceRecordID: proj.ID,
		Locator:        recall.Locator{SourceID: p.sourceID, Local: proj.ID},
		RecordType:     RecordProject,
		Title:          title(proj),
		Excerpt:        clip(summarize(proj), excerptBytes),
		LocalRank:      rank,
		LocalScore:     &local,
		Relevance:      &rel,
		MatchSignals:   signals,
		ObservedAt:     &p.observed,
		EventTime:      proj.eventTime(),
		SourceRevision: p.cat.revision(),
		Sensitivity:    p.floor,
		Metadata:       metadata(proj),
		// Two instances of this adapter commonly point at ONE ongoing server
		// with different `views` — "everything" and "the things that need
		// attention" are different questions over the same catalog. Lineage
		// groups on source_uid + source_record_id, so without a fingerprint
		// those instances hand the core one project twice under two identities,
		// and it is counted as two independent pieces of evidence: the
		// corroboration bonus then promotes exactly the projects that appear in
		// the most views. A record read once, from one endpoint, at one scan
		// revision, is one observation. The fingerprint says so, and
		// record_type + content_fingerprint collapses them without the core
		// needing to know anything about ongoing.
		ContentFingerprint: fingerprint(proj.ID, p.cat.revision()),
	}
	if finished, ok := p.cat.boundary(); ok {
		// The catalog's last complete scan is the only boundary that confirmed
		// this project present. The per-project lastSeenAt is that same scan's
		// sighting, so the two agree; the scan is quoted because it is the
		// boundary, and a sighting inside an incomplete pass confirms nothing.
		c.ConfirmedAt = &finished
	}
	return c
}

// title is what a reader sees before deciding to expand. The path is in it
// because two scan roots can hold two projects called "www".
func title(p *project) string {
	name, rel := oneLine(p.Name), oneLine(p.RelativePath)
	if rel != "" && rel != name {
		return name + " (" + rel + ")"
	}
	return name
}

// summarize is the candidate preview: the owner's note, then every
// classification this project is in with the reasons that put it there.
//
// This is the sentence the ticket for this adapter was written around. A label
// on its own is noise — "dormant" says nothing a person can act on — and the
// reasons are what make it a decision.
func summarize(p *project) string {
	parts := make([]string, 0, 4)
	if note := oneLine(p.Note); note != "" {
		parts = append(parts, note)
	}
	if p.IsMissing {
		parts = append(parts, "missing from disk")
	}
	for _, key := range p.memberViews() {
		messages := make([]string, 0, 4)
		for _, r := range p.reasonsFor(key) {
			messages = append(messages, oneLine(r.Message))
		}
		if len(messages) == 0 {
			parts = append(parts, viewLabels[key])
			continue
		}
		parts = append(parts, viewLabels[key]+": "+strings.Join(messages, "; "))
	}
	if len(parts) == 0 {
		// A project in no classification is not a problem to report; it is a
		// project that is simply fine. Say what it is instead of saying nothing.
		if p.Metrics != nil && p.Metrics.LatestCommitSubject != "" {
			return "latest commit: " + oneLine(p.Metrics.LatestCommitSubject)
		}
		return "no attention classification"
	}
	return strings.Join(parts, " · ")
}

// metadata is the typed fields a caller routes and displays on.
//
// The attention reasons are the point of this source and travel whole — input,
// value, comparison, threshold — in ongoing's own vocabulary. Everything else
// is a measurement the catalog holds, carried as a number rather than flattened
// into the excerpt, which is what docs/spec.md asks of a structured source. A
// measurement the catalog does not have is absent rather than zero: "never
// scanned" and "no commits" are different facts.
func metadata(p *project) map[string]any {
	md := map[string]any{
		// Paths are source text too: ongoing records whatever the filesystem
		// hands it, and a directory name can carry line structure.
		"path":          oneLine(p.CanonicalPath),
		"relative_path": oneLine(p.RelativePath),
	}
	if views := p.memberViews(); len(views) > 0 {
		md["views"] = views
		md["attention_reasons"] = reasonsPayload(p)
	}
	if p.IsFavorite {
		md["favorite"] = true
	}
	if p.IsMissing {
		md["missing"] = true
	}
	if p.Intent != nil {
		text(md, "intent", *p.Intent)
	}
	if p.NextAction != nil {
		text(md, "next_action", *p.NextAction)
	}
	// The owner's own two ratings, kept apart. Combining them into one number
	// would be the priority score this source refuses to invent.
	number(md, "excitement", p.Excitement)
	number(md, "strategic_importance", p.StrategicImportance)
	if n := len(p.Errors); n > 0 {
		md["collector_warnings"] = n
	}
	m := p.Metrics
	if m == nil {
		return md
	}
	text(md, "branch", m.Branch)
	text(md, "latest_commit_subject", m.LatestCommitSubject)
	stamp(md, "latest_commit_at", m.LatestCommitAt)
	number(md, "commits_30d", m.Commits30d)
	number(md, "active_days_30d", m.ActiveDays30d)
	number(md, "loc_code", m.LocCode)
	text(md, "dominant_language", m.DominantLanguage)
	number(md, "td_open", m.TdOpenCount)
	number(md, "td_blocked", m.TdBlockedCount)
	number(md, "td_stale", m.TdStaleCount)
	if repo := repository(m); repo != "" {
		md["github_repo"] = repo
	}
	number(md, "github_stars", m.GithubStars)
	number(md, "github_open_issues", m.GithubOpenIssues)
	number(md, "github_external_prs", m.GithubExternalPrs)
	text(md, "github_ci_state", m.GithubCiState)
	number(md, "github_traffic_views", m.GithubTrafficViews)
	return md
}

// reasonsPayload flattens the recorded reasons across every classification the
// project is in, tagging each with the view that recorded it. The four
// evidence fields travel unchanged and untyped-as-given: a threshold ongoing
// wrote as the string "1–5" stays that string.
func reasonsPayload(p *project) []map[string]any {
	out := make([]map[string]any, 0, 6)
	for _, key := range p.memberViews() {
		for _, r := range p.reasonsFor(key) {
			out = append(out, map[string]any{
				// Every string here is source text on its way to a
				// terminal and a model. The core's sanitizer walks top-level
				// string metadata only, so anything nested one level down —
				// which this is — arrives exactly as ongoing stored it unless
				// it is cleaned here. Non-string values keep their type: a
				// null intent stays null, "1-5" stays a string.
				"view":       oneLine(key),
				"source":     oneLine(r.Source),
				"message":    oneLine(r.Message),
				"input":      safeAny(r.Input),
				"value":      safeAny(r.Value),
				"comparison": safeAny(r.Comparison),
				"threshold":  safeAny(r.Threshold),
			})
		}
	}
	return out
}

func repository(m *metrics) string {
	if m.GithubOwner == "" || m.GithubName == "" {
		return ""
	}
	return m.GithubOwner + "/" + m.GithubName
}

// keepsRequest applies the filters that came with this request, as opposed to
// the instance's configured scoping.
func keepsRequest(p *project, f recall.Filters) bool {
	if len(f.RecordTypes) > 0 && !wantsProjects(f.RecordTypes) {
		return false
	}
	if f.Project != "" && !isProject(p, f.Project) {
		return false
	}
	if len(f.Entities) > 0 && !mentionsEntity(p, f.Entities) {
		return false
	}
	if f.Since != nil || f.Until != nil {
		// The only time a project carries is its latest commit. A project that
		// has never been committed to, or whose git scan failed, cannot be
		// shown to fall inside the window, so a window excludes it rather than
		// admitting it on the assumption that undated means always.
		at := p.eventTime()
		if at == nil {
			return false
		}
		if f.Since != nil && at.Before(*f.Since) {
			return false
		}
		if f.Until != nil && at.After(*f.Until) {
			return false
		}
	}
	return true
}

func wantsProjects(want []recall.RecordType) bool {
	for _, t := range want {
		if t == RecordProject {
			return true
		}
	}
	return false
}

// isProject matches the core's project filter against the names this catalog
// actually files a repository under.
func isProject(p *project, want string) bool {
	for _, name := range []string{p.Name, p.RelativePath, p.ID} {
		if name != "" && strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

// mentionsEntity matches a requested entity against the entity-shaped handles a
// project has: the names it is filed under and the repository it publishes as.
func mentionsEntity(p *project, entities []string) bool {
	handles := []string{p.Name, p.RelativePath, p.ID}
	if p.Metrics != nil {
		handles = append(handles, p.Metrics.GithubOwner, p.Metrics.GithubName, repository(p.Metrics))
	}
	for _, entity := range entities {
		for _, handle := range handles {
			if handle != "" && strings.EqualFold(handle, entity) {
				return true
			}
		}
	}
	return false
}

// match scores a project against the query terms and reports whether any term
// matched a stable identifier at a token boundary. The score is the mean weight
// per term, so a long query is not scored above a precise one.
//
// A term is weighed under its own spelling or a discounted number variant of
// it, on the one definition every source shares: see [recall.WeighTerm].
func match(p *project, terms []string, variants recall.TermVariants) (score float64, exact bool) {
	if len(terms) == 0 {
		return 0, false
	}
	weights := weigh(p)
	for _, term := range terms {
		exact = exact || identifies(p, term)
		score += variants.Weigh(weights, term)
	}
	return score / float64(len(terms)), exact
}

// identifies reports whether term is one of this project's stable identifiers,
// compared whole. An unbounded substring match never counts, which is what
// keeps exact_identifier meaning what the protocol says it means.
func identifies(p *project, term string) bool {
	for _, id := range []string{p.ID, p.Name, p.RelativePath} {
		if id != "" && strings.EqualFold(id, term) {
			return true
		}
	}
	return false
}

// weigh tokenizes one project's searchable fields.
//
// A token appearing in several fields keeps the strongest weight: a name hit
// says more about the project than a hit inside an attention reason. The map is
// built per search rather than cached, because this source holds no projection
// to cache it in — every search reads the catalog fresh, and a hundred projects
// is a few milliseconds of tokenizing.
func weigh(p *project) map[string]float64 {
	w, _, _ := weighAndCount(p)
	return w
}

// weighAndCount walks this project's searchable text once and returns all three
// things a search needs from it: the per-token weight a match earns, how often
// each token occurs, and the total length in tokens.
//
// They are gathered in one traversal because they are three readings of one
// field list. Kept apart, the list would be written twice and a field added to
// one copy and missed in the other would silently change relevance with nothing
// failing — weights forgets every occurrence after the strongest, so it cannot
// answer the other two questions, and a second walk is exactly the drift this
// avoids.
func weighAndCount(p *project) (weights map[string]float64, counts map[string]int, length int) {
	weights, counts = map[string]float64{}, map[string]int{}
	add := func(text string, weight float64) {
		for _, token := range tokenize(text) {
			if weights[token] < weight {
				weights[token] = weight
			}
			counts[token]++
			length++
		}
	}
	add(p.Name, weightName)
	add(p.RelativePath, weightPath)
	add(p.CanonicalPath, weightFullPath)
	add(p.Note, weightDecision)
	if p.NextAction != nil {
		add(*p.NextAction, weightDecision)
	}
	if p.Intent != nil {
		add(*p.Intent, weightAttribute)
	}
	for _, key := range p.memberViews() {
		add(key, weightAttribute)
		add(viewLabels[key], weightAttribute)
		for _, r := range p.reasonsFor(key) {
			add(r.Message, weightNarrative)
			add(r.Input, weightNarrative)
		}
	}
	if m := p.Metrics; m != nil {
		add(m.DominantLanguage, weightAttribute)
		add(m.Branch, weightAttribute)
		add(m.GithubOwner, weightAttribute)
		add(m.GithubName, weightAttribute)
		add(m.LatestTag, weightAttribute)
		add(m.LatestCommitSubject, weightNarrative)
	}
	return weights, counts, length
}

// relevance is [recall.Candidate.Relevance] for one project.
//
// This is the number whose absence let five unrelated projects outrank the
// document answering a question about a retinal detachment: this source scored
// them 0.08 and had no way to say so across the boundary.
func relevance(p *project, terms []string, variants recall.TermVariants) float64 {
	_, counts, length := weighAndCount(p)
	return variants.RelevanceOverCounts(terms, counts, length)
}

// newerCommit orders two projects by most recent commit, then by name, then by
// id, so the comparison is total and two searches never disagree about the
// order of the same pair. It is ongoing's own default dashboard sort.
func newerCommit(l, r *project) bool {
	lt, rt := l.eventTime(), r.eventTime()
	switch {
	case lt != nil && rt != nil && !lt.Equal(*rt):
		return lt.After(*rt)
	case lt != nil && rt == nil:
		// A project with a known commit date sorts above one with none. An
		// unknown date is not "long ago"; it is unknown, and guessing would put
		// an unscanned project at the bottom as if it were abandoned.
		return true
	case lt == nil && rt != nil:
		return false
	case l.Name != r.Name:
		return l.Name < r.Name
	default:
		return l.ID < r.ID
	}
}

// tokenize splits on anything that cannot appear inside an identifier.
//
// Identifier punctuation stays inside the token, so "epub_to_audiobook" and
// "iTerm2-Color-Schemes" are each one token and can be compared for equality
// against a project name. A tokenizer that split on "-" would make exact
// identifier matching impossible for most of this catalog.
func tokenize(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
		switch r {
		case '-', '_', '.', '/':
			return false
		default:
			return true
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.Trim(f, "-_./"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// queryMode names how this search was answered, for diagnostics.
//
// A query with no terms is not "no retrieval": the filters and the instance's
// configured views selected a set, and the catalog's own ordering ranked it.
// That is a structured query, unless a time window made it a temporal one.
func queryMode(terms []string, exact bool, f recall.Filters) string {
	switch {
	case exact:
		return "exact"
	case len(terms) > 0:
		return "lexical"
	case f.Since != nil || f.Until != nil:
		return "temporal"
	default:
		return "structured"
	}
}

// fingerprint identifies the observation, not the text. Two instances reading
// one catalog at one revision saw the same thing, whatever each chose to
// render, so the digest covers the record identity and the scan revision and
// deliberately not the excerpt — a fingerprint over rendered text would differ
// between a full-catalog instance and a view-filtered one and defeat itself.
func fingerprint(projectID, revision string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + revision))
	return hex.EncodeToString(sum[:])
}

// safeAny cleans a value that may or may not be a string. Attention reasons
// carry their inputs and thresholds as ongoing typed them — a number stays a
// number, a null stays null — so this touches only the string case and leaves
// everything else identical.
func safeAny(v any) any {
	if s, ok := v.(string); ok {
		return oneLine(s)
	}
	return v
}
