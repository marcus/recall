package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/internal/explain"
	"github.com/marcus/recall/internal/recall"
	"github.com/marcus/recall/internal/source"
)

const queryHelp = `usage: recall query [flags] <text>

Search every eligible source and fuse what comes back. The query text is the
remaining arguments, joined by spaces.

flags:
  --profile NAME       profile to resolve; default is the configured one
  --json               emit the response as JSON, projected to the pointer
                       tier: the outcome, one pointer per result, the source
                       summary, and anything suppressed or omitted
  --explain            add the diagnostic tier: per-result provenance and
                       cluster lineage, score explanations, per-source
                       outcomes, and the resolved retrieval plan. Combines
                       with --json, and --json --explain is the complete
                       serialization — every field, nothing projected
  --limit N            maximum fused results. Unset AND 0 both mean the
                       profile's max_results; for an unbounded answer set
                       max_results = 0 in [defaults], or name a larger number
  --budget-ms N        latency budget for the whole request
  --budget-tokens N    size budget for the whole rendered response, footers
                       included; trailing results compress, then drop. Default
                       8000; -1 is unbounded
  --scope KEY=VALUE    narrow the request; repeatable and comma-separable.
                       Keys: source, type, project, entity, since, until.
                       Times are RFC 3339.
  --as-of TIME         answer as of a past instant (RFC 3339). Sources that
                       cannot honor a historical boundary are excluded and
                       reported, and coverage becomes degraded.
  --server URL         dispatch to a running recall serve instance
  --auth-token-env ENV read the server bearer token from ENV

A default result is a pointer: rank, locator, title, and the excerpt, marked
"exact" when the query named the record outright and "corroborated N" when the
result stands on more than one independent record. The locator is the handle
recall expand takes, and choosing what to expand is what the list is for.
Scores, provenance, lineage, source outcomes, and the plan are diagnostics —
behind --explain on either encoding, and answered directly by recall sources
and recall doctor when they are the question.

The tier is --explain and the encoding is --json, and they are independent:

  (neither)         pointers, for a person
  --explain         pointers plus diagnostics, for a person
  --json            pointers, as JSON: tier "pointer"
  --json --explain  the complete serialization, every field, nothing projected

--json alone no longer emits every field. It did until the pointer projection
existed, and the fields it dropped are the diagnostic tier and nothing else:
score, explanation, cluster members, per-result provenance, the per-source
ledger, and the plan. Reach them with --json --explain, which is byte for byte
what --json used to print. What --json still carries unconditionally is what
the response CLAIMS — the outcome, the coverage, a summary naming any source
that could not answer, every suppression, and anything a budget omitted — on
the same rule the human surface follows: a fact whose absence would read as an
answer having nothing more to give is not a fact any tier may drop.

Every response is bounded. --budget-tokens is charged against the whole
rendering — the outcome line, each result as this surface prints it, and the
source and plan footers when --explain prints them — so the number means the
same thing on every surface, and a serialized response costs what serializing
it costs. Unset it is 8000 tokens, roughly 32 KB of human output; -1 removes
the ceiling. A budget too small for the footers summarizes them rather than
ignoring them, and says so. What it never drops is the outcome, the coverage,
a degraded source, or a suppression: those are what the answer claims.

How long the list is, is two rules and both are configuration. The profile's
relevance_floor withholds a record its own source reports as barely about the
query; max_results is the budget the rest compete for, across every source at
once, so adding a source changes which records reach you and not how many.
Neither is a coverage claim — every eligible source was still asked — and what
each held back is counted in "suppressed" and in "dropped". --explain prints
both values in the plan.

Human output states coverage inline: a source that was eligible and could not
answer is named, never silently absent. An excerpt is marked by what it is:
"> " is the span that matched the query, "~ " is the record's opening shown
because nothing in its text matched, and an unmarked excerpt is one whose
source did not say which. --json reports the same as excerpt_kind.

` + exitCodes

func runQuery(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("query")
	var (
		profile   = fs.String("profile", "", "profile to resolve")
		asJSON    = fs.Bool("json", false, "emit JSON")
		explained = fs.Bool("explain", false, "show score explanations")
		limit     = fs.Int("limit", 0, "maximum fused results")
		budgetMS  = fs.Int("budget-ms", 0, "latency budget in milliseconds")
		tokens    = fs.Int("budget-tokens", 0, "response token budget")
		asOfText  = fs.String("as-of", "", "historical boundary, RFC 3339")
	)
	var scopes multiFlag
	fs.Var(&scopes, "scope", "narrow the request: source=, type=, project=, entity=, since=, until=")
	remote := addRemoteFlags(fs)

	if ok, code := parse(env, fs, queryHelp, args); !ok {
		return code
	}
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return usageErr(env, queryHelp, fmt.Errorf("no query text"))
	}

	// Refused here rather than reaching fusion, which would surface a
	// configuration error for something the caller typed. There is deliberately
	// no negative-means-unbounded spelling to match --budget-tokens: a result
	// budget is profile policy and the HTTP surface refuses a negative limit
	// outright, so accepting one here would be a capability the CLI had and no
	// other transport did. The escape is named instead of guessed at.
	if *limit < 0 {
		return usageErr(env, queryHelp, fmt.Errorf(
			"--limit %d: a limit is not negative; for an unbounded answer set "+
				"max_results = 0 in [defaults], or name a number larger than the "+
				"profile's", *limit))
	}

	scope, err := parseScope(scopes)
	if err != nil {
		return usageErr(env, queryHelp, err)
	}
	asOf, err := parseAsOf(*asOfText)
	if err != nil {
		return usageErr(env, queryHelp, err)
	}

	core, closeCore, err := openCore(env, *profile, *limit, remote)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Query(ctx, recall.QueryRequest{
		Query:   text,
		Profile: *profile,
		Scope:   scope,
		AsOf:    asOf,
		Mode:    recall.ModeExplicit,
		Budget: recall.Budget{
			LatencyMS:      *budgetMS,
			ResponseTokens: responseTokens(*tokens),
			Surface:        surface(*asJSON, *explained),
		},
		Limit: *limit,
	})
	if err != nil {
		// A request that could not be planned or fused made no claim about the
		// corpus at all, so it is an error and never an empty answer.
		fail(env, err)
		return ExitError
	}

	if *asJSON {
		// --explain is the same request on this surface as on the human one:
		// add the diagnostic tier. Without it the response is projected to
		// pointers, which is what a caller choosing a locator to expand reads.
		var body any = projectPointer(resp)
		if *explained {
			body = resp
		}
		if code := report(env, emitJSON(env.Stdout, body)); code != ExitOK {
			return code
		}
		return queryExit(resp)
	}
	var o out
	renderQuery(&o, resp, *explained)
	if code := report(env, o.flush(env.Stdout)); code != ExitOK {
		return code
	}
	return queryExit(resp)
}

// responseTokens resolves what an unset --budget-tokens means here.
//
// It is the default ceiling, not unbounded. The core leaves an unset budget
// unbounded because a library caller pays no rendering cost for a struct it
// holds; a command whose output lands in a terminal or a context window is the
// other case, and the observed failure this exists to stop was a single query
// that printed 203 KB nobody asked for. A negative value is the caller saying
// unbounded outright, which is the escape hatch a ceiling needs in order to be
// a default rather than a limit.
func responseTokens(asked int) int {
	if asked == 0 {
		return recall.DefaultResponseTokens
	}
	return asked
}

// surface names the rendering this invocation will print, so the budget is
// charged against what the caller actually receives.
//
// The two flags are orthogonal and both matter: --json picks the encoding,
// --explain picks the tier, and the four combinations are four different sizes.
// Pricing projected JSON as the whole serialization would be safe in the sense
// that it never underestimates, and wrong in the sense that matters — the
// budget would shape the response for bytes this invocation is not going to
// print, and a machine caller would get a shorter answer than a person running
// the same query.
func surface(asJSON, explained bool) recall.ResponseSurface {
	switch {
	case asJSON && explained:
		return recall.SurfaceStructured
	case asJSON:
		return recall.SurfaceStructuredPointer
	case explained:
		return recall.SurfaceExplained
	default:
		return recall.SurfacePointer
	}
}

// queryExit maps one response onto an exit code, most severe first.
//
// Outcome and coverage are orthogonal, so a single status has to order them. A
// degraded run that abstained exits degraded rather than abstained: the
// abstention is a claim about the corpus, and an incomplete set of sources does
// not support one.
func queryExit(resp recall.QueryResponse) int {
	switch {
	case resp.Outcome == recall.OutcomeFailed:
		return ExitFailed
	case resp.Coverage == recall.CoverageDegraded:
		return ExitDegraded
	case resp.Outcome == recall.OutcomeAbstained:
		return ExitAbstained
	default:
		return ExitOK
	}
}

// renderQuery writes the human form of a response in two tiers.
//
// The default tier is a projection for a reader with finite attention: the
// outcome, one pointer per result, and the two things that must never be
// missing — degraded coverage and suppression. The diagnostic tier — per-result
// provenance, cluster lineage, score explanations, per-source outcomes, and the
// resolved plan — is behind --explain. Nothing becomes unreachable: --json
// carries every field unprojected, and docs/spec.md records why a projected
// human surface still satisfies the human/JSON parity invariant.
//
// One flag rather than two. --explain already means "why did this answer come
// out this way", and every block behind it answers that question; a second
// --verbose would split the diagnostics across two axes and leave a caller
// learning which flag holds which fact. It is also the flag a caller already
// reaches for, so nothing new has to be discovered to get the old output back.
//
// Every block below is its own function because the response budget is charged
// by running them: cost.go prices a frame and a result by rendering one, so
// what the budget buys and what the surface prints cannot drift apart.
func renderQuery(o *out, resp recall.QueryResponse, explained bool) {
	renderOutcome(o, resp)
	renderResults(o, resp, explained)
	renderSuppressed(o, resp.Suppressed)
	if explained {
		renderSourceOutcomes(o, resp)
		renderPlan(o, resp)
	}
}

// renderOutcome states what the run claims about the corpus. It is never
// flagged: a person running a query without --json is exactly the person who
// must not be told a partial answer is a whole one.
func renderOutcome(o *out, resp recall.QueryResponse) {
	var head fields
	head.text("outcome", string(resp.Outcome))
	head.text("coverage", string(resp.Coverage))
	head.count("results", len(resp.Results))
	head.dur("elapsed", resp.Elapsed)
	head.flag("truncated", resp.Truncated)
	head.count("dropped", resp.DroppedResults)
	o.line(head.String())

	if degraded := degradedSources(resp); len(degraded) > 0 {
		// The one line that must never be missing from human output: a source
		// that could not answer is named here, not left to be inferred from an
		// absence further down.
		o.line("degraded coverage: " + strings.Join(degraded, ", "))
	}
}

// degradedSources names the sources that could not answer, from the ledger or
// from the summary standing in for it. This line does not shrink under budget
// pressure: it is the response's own claim about how complete it is.
func degradedSources(resp recall.QueryResponse) []string {
	if len(resp.SourceOutcomes) == 0 && resp.SourceSummary != nil {
		return resp.SourceSummary.Degraded
	}
	return source.DegradedReports(resp.SourceOutcomes)
}

func renderResults(o *out, resp recall.QueryResponse, explained bool) {
	results := resp.Results
	o.blank()
	if len(results) == 0 {
		if resp.DroppedResults > 0 {
			// "none" would read as "nothing matched" when a budget or a limit
			// is what emptied the list, and that is the one thing an empty
			// result set must never be confused with. Which of the two it was
			// is not claimed here: the count is what is known.
			o.line(fmt.Sprintf("results: none shown, %d dropped", resp.DroppedResults))
			return
		}
		o.line("results: none")
		return
	}
	for i, r := range results {
		renderResult(o, i+1, r)
		if explained {
			renderResultDetail(o, r)
		}
	}
}

// renderResult is the default tier of one result: what a caller needs to decide
// whether to expand this locator, and nothing that only says how it got here.
// The locator carries the source, so naming the source again would be a field
// spent restating the line above it. The fusion score is not here either: it is
// ordinal and uncalibrated, so the position in the list is the whole of what it
// says, and explain.Render prints the number with the arithmetic behind it.
func renderResult(o *out, rank int, r recall.Result) {
	var head fields
	// Two markers earn their place because each states something the rank does
	// not. "exact" says the query named this record outright, which is what
	// makes a "~" excerpt — nothing in the text matched — a strong result
	// rather than a suspicious one.
	head.flag("exact", r.Explanation.ExactPromoted)
	if n := r.Explanation.Corroboration.IndependentUnits; n > 1 {
		// Independent records agreeing is the one fact the lineage blocks carry
		// that the locator does not, and it is the reason to run this instead
		// of grep. Units, not members: a record that arrived as three chunks
		// corroborates nothing.
		head.count("corroborated", n)
	}
	line := fmt.Sprintf("%d. %s", rank, r.Primary.Locator.String())
	if !head.empty() {
		line += "  " + head.String()
	}
	o.line(line)
	if r.Primary.Title != "" {
		o.block("   ", r.Primary.Title)
	}
	if r.Primary.Excerpt != "" {
		o.block(excerptIndent(r.Primary.ExcerptKind), r.Primary.Excerpt)
	}
}

// renderResultDetail is the diagnostic tier of one result: the record's
// provenance, the cluster's lineage, and the score explanation. All of it
// answers "why is this here and what exactly is it", which is what --explain
// asks, and all of it is in --json unconditionally.
func renderResultDetail(o *out, r recall.Result) {
	var meta fields
	meta.text("type", string(r.Primary.RecordType))
	meta.text("sensitivity", r.Primary.Sensitivity.String())
	meta.text("id", r.Primary.SourceRecordID)
	meta.text("candidate", r.Primary.CandidateID)
	meta.text("revision", r.Primary.SourceRevision)
	meta.text("fingerprint", r.Primary.ContentFingerprint)
	meta.text("signals", join(r.Primary.MatchSignals))
	meta.count("local rank", r.Primary.LocalRank)
	if r.Primary.LocalScore != nil {
		meta.text("local score", num(*r.Primary.LocalScore))
	}
	meta.at("event", r.Primary.EventTime)
	meta.at("valid from", r.Primary.ValidFrom)
	meta.at("valid to", r.Primary.ValidTo)
	meta.at("observed", r.Primary.ObservedAt)
	meta.at("confirmed", r.Primary.ConfirmedAt)
	meta.text("derived from", locators(r.Primary.DerivedFrom))
	meta.text("metadata", diagnostics(r.Primary.Metadata))

	if !meta.empty() {
		o.block("   ", meta.String())
	}
	renderMembers(o, r.Members)
	o.block("   ", explain.Render(r.Explanation))
}

// excerptIndent marks what the excerpt is, in the three states the JSON form
// carries: "> " quotes the span the query matched, "~ " stands in for it with
// the record's opening because nothing in its text matched, and no marker means
// the source claimed neither.
//
// All three are one glyph wide so the block stays aligned with everything else
// under a result. A marker rather than a label because the distinction has to
// survive without --explain: a caller shown the head of a document has no way
// to tell a real hit from a false one, and establishing it by hand is what that
// costs. The unmarked case is a third state and not a preview — collapsing the
// two would make the human surface assert something the JSON does not.
func excerptIndent(kind recall.ExcerptKind) string {
	switch kind {
	case recall.ExcerptMatched:
		return " > "
	case recall.ExcerptPreview:
		return " ~ "
	default:
		return "   "
	}
}

// renderMembers shows the cluster's lineage groups. Two candidates inside one
// member mean one record seen twice, and collapsing them would turn
// corroboration into repetition. Two members mean two roots, which is two
// records unless the suppression block reports one as a duplicate view of the
// other; the corroborated count above is what says which.
func renderMembers(o *out, members []recall.ClusterMember) {
	for _, m := range members {
		o.block("   ", "lineage "+string(m.LineageRoot))
		for _, c := range m.Candidates {
			var f fields
			f.text("source", c.SourceID)
			f.count("local rank", c.LocalRank)
			f.text("signals", join(c.MatchSignals))
			o.block("     ", c.Locator.String()+"  "+f.String())
		}
	}
}

func locators(in []recall.Locator) string {
	if len(in) == 0 {
		return ""
	}
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = l.String()
	}
	return strings.Join(out, ", ")
}

// renderSourceOutcomes lists every source the request touched, answered or
// not. A source that failed is reported here rather than being absent.
//
// Behind --explain, because this is the whole per-source ledger and it does not
// shrink with the result set: it was the entire cost of a query that found
// nothing. The failure a caller must not miss is already named unflagged by
// renderOutcome, and recall sources answers the rest on demand.
// When the response budget could not afford the ledger, the summary that stood
// in for it is printed instead, marked as the stand-in it is. The alternative —
// printing nothing under a "sources" heading — would say this profile has no
// sources, which is a different and false statement.
func renderSourceOutcomes(o *out, resp recall.QueryResponse) {
	reports := resp.SourceOutcomes
	o.blank()
	o.line("sources")
	if len(reports) == 0 && resp.SourceSummary != nil {
		s := resp.SourceSummary
		var f fields
		f.count("sources", s.Sources)
		for _, outcome := range slices.Sorted(maps.Keys(s.Outcomes)) {
			f.count(string(outcome), s.Outcomes[outcome])
		}
		o.block("  ", f.String()+"  (per-source ledger omitted for the response budget)")
		return
	}
	if len(reports) == 0 {
		o.block("  ", "none configured for this profile")
		return
	}
	for _, r := range reports {
		var f fields
		f.text("outcome", string(r.Outcome))
		f.text("reason", r.Reason)
		f.count("candidates", r.Candidates)
		f.dur("elapsed", r.Elapsed)
		f.dur("cold start", r.ColdStart)
		f.text("watermark", r.SourceWatermark)
		f.text("generation", r.IndexGeneration)
		f.at("confirmed", r.ConfirmedAt)
		f.text("diagnostics", diagnostics(r.Diagnostics))
		o.block("  ", fmt.Sprintf("%s (%s)  %s", r.SourceID, r.SourceUID, f.String()))
	}
}

// renderPlan is the resolved retrieval plan, behind --explain: it describes the
// request rather than the answer, and it is identical for every query against
// the profile.
//
// Its header is one line and stays whatever the budget is. The per-source list
// grows with the profile, so it is what a budget drops, and it says so rather
// than reading as a plan that reached no sources.
func renderPlan(o *out, resp recall.QueryResponse) {
	plan := resp.Plan
	o.blank()
	var head fields
	head.text("profile", plan.Profile)
	// The two volume rules print even at zero, where every other count and
	// number here is omitted. Zero is what an operator writes to turn one of
	// them off — unbounded results, no floor — and omitting it would render a
	// rule somebody deliberately disabled identically to one this tier never
	// reports, which is the difference `--explain` exists to show.
	head.raw("limit " + strconv.Itoa(plan.Limit))
	head.number("rank constant", plan.RankConst)
	head.number("corroboration cap", plan.Corrobor)
	head.raw("relevance floor " + num(plan.RelevanceFloor))
	head.dur("fusion reserve", plan.Reserve)
	if !plan.Deadline.IsZero() {
		head.text("deadline", stamp(plan.Deadline))
	}
	o.line("plan  " + head.String())

	if len(plan.Sources) == 0 && slices.Contains(resp.Omitted, recall.OmittedPlanSources) {
		o.block("  ", "per-source plan omitted for the response budget")
		return
	}
	for _, s := range plan.Sources {
		var f fields
		f.raw(eligibility(s.Eligible))
		f.text("reason", s.Reason)
		f.count("limit", s.Limit)
		f.dur("timeout", s.Timeout)
		f.number("prior", s.Prior)
		o.block("  ", fmt.Sprintf("%s (%s)  %s", s.SourceID, s.SourceUID, f.String()))
	}
}

// renderSuppressed reports what was withheld. Every suppressed candidate is
// counted with a reason so a host can say something was not shown without
// saying what it was. Unflagged, for the same reason the coverage line is:
// silence about a withheld record reads as an answer that had nothing more.
func renderSuppressed(o *out, in []recall.Suppression) {
	if len(in) == 0 {
		return
	}
	o.blank()
	o.line("suppressed")
	for _, s := range in {
		var f fields
		f.count("count", s.Count)
		f.text("lineage", string(s.LineageRoot))
		o.block("  ", s.Reason+"  "+f.String())
	}
}

// multiFlag collects a repeatable flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseScope reads the explicit scope filters. Scope is a hard eligibility
// constraint, so an unreadable one fails the command rather than being ignored:
// a mistyped filter that silently widened a request would search sources the
// caller believed it had excluded.
func parseScope(in []string) (*recall.Scope, error) {
	scope := &recall.Scope{}
	set := false
	for _, entry := range in {
		for _, part := range strings.Split(entry, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, value, ok := strings.Cut(part, "=")
			if !ok || value == "" {
				return nil, fmt.Errorf("scope %q: want key=value, one of source, type, project, entity, since, until", part)
			}
			switch key {
			case "source":
				scope.SourceIDs = append(scope.SourceIDs, value)
			case "type":
				scope.RecordTypes = append(scope.RecordTypes, recall.RecordType(value))
			case "entity":
				scope.Entities = append(scope.Entities, value)
			case "since":
				t, err := parseTime(key, value)
				if err != nil {
					return nil, err
				}
				scope.Since = t
			case "until":
				t, err := parseTime(key, value)
				if err != nil {
					return nil, err
				}
				scope.Until = t
			case "project":
				if scope.Project != "" && !strings.EqualFold(scope.Project, value) {
					// Two projects is not a narrower request, it is a
					// different one, and the sources evaluate this field
					// singly. Refusing beats silently keeping the last.
					return nil, fmt.Errorf("scope project: %q and %q; a request names one project",
						scope.Project, value)
				}
				scope.Project = value
			default:
				return nil, fmt.Errorf("scope key %q: want one of source, type, project, entity, since, until", key)
			}
			set = true
		}
	}
	if !set {
		return nil, nil
	}
	return scope, nil
}

func parseAsOf(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	return parseTime("as-of", v)
}

func parseTime(field, v string) (*time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("%s %q: want an RFC 3339 time such as %s",
			field, v, time.Now().UTC().Format(time.RFC3339))
	}
	return &t, nil
}

func eligibility(ok bool) string {
	if ok {
		return "eligible"
	}
	return "ineligible"
}
