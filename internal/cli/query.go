package cli

import (
	"context"
	"fmt"
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
  --json               emit the response as JSON
  --explain            show each result's score explanation
  --limit N            maximum fused results
  --budget-ms N        latency budget for the whole request
  --budget-tokens N    response size budget; trailing results compress, then drop
  --scope KEY=VALUE    narrow the request; repeatable and comma-separable.
                       Keys: source, type, project, entity, since, until.
                       Times are RFC 3339.
  --as-of TIME         answer as of a past instant (RFC 3339). Sources that
                       cannot honor a historical boundary are excluded and
                       reported, and coverage becomes degraded.
  --server URL         dispatch to a running recall serve instance
  --auth-token-env ENV read the server bearer token from ENV

Human output states coverage inline: a source that was eligible and could not
answer is named, never silently absent.

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
		Budget:  recall.Budget{LatencyMS: *budgetMS, ResponseTokens: *tokens},
		Limit:   *limit,
	})
	if err != nil {
		// A request that could not be planned or fused made no claim about the
		// corpus at all, so it is an error and never an empty answer.
		fail(env, err)
		return ExitError
	}

	if *asJSON {
		if code := report(env, emitJSON(env.Stdout, resp)); code != ExitOK {
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

// renderQuery writes the tiered human form of a response.
//
// It carries every fact the JSON form carries. That is not a courtesy to the
// terminal: the spec says the same structure serializes to JSON and renders as
// tiered text, and neither surface gets extra fields. Degraded coverage in
// particular is stated inline, because a person running a query without --json
// is exactly the person who must not be told a partial answer is a whole one.
func renderQuery(o *out, resp recall.QueryResponse, explained bool) {
	var head fields
	head.text("outcome", string(resp.Outcome))
	head.text("coverage", string(resp.Coverage))
	head.count("results", len(resp.Results))
	head.dur("elapsed", resp.Elapsed)
	head.flag("truncated", resp.Truncated)
	head.count("dropped", resp.DroppedResults)
	o.line(head.String())

	if degraded := degradedSources(resp.SourceOutcomes); len(degraded) > 0 {
		// The one line that must never be missing from human output: a source
		// that could not answer is named here, not left to be inferred from an
		// absence further down.
		o.line("degraded coverage: " + strings.Join(degraded, ", "))
	}

	renderResults(o, resp.Results, explained)
	renderSourceOutcomes(o, resp.SourceOutcomes)
	renderPlan(o, resp.Plan)
	renderSuppressed(o, resp.Suppressed)
}

func degradedSources(reports []recall.SourceReport) []string {
	var out []string
	for _, r := range reports {
		degrades := r.Outcome.Degrades()
		if r.Outcome == recall.SearchSkipped {
			degrades = source.Degrades(r.Reason)
		}
		if !degrades {
			continue
		}
		reason := r.Reason
		if reason == "" {
			reason = string(r.Outcome)
		}
		out = append(out, fmt.Sprintf("%s (%s)", r.SourceID, reason))
	}
	return out
}

func renderResults(o *out, results []recall.Result, explained bool) {
	o.blank()
	if len(results) == 0 {
		o.line("results: none")
		return
	}
	for i, r := range results {
		var head fields
		head.text("score", num(r.Score))
		head.flag("exact", r.Explanation.ExactPromoted)
		o.printf("%d. %s  %s\n", i+1, r.Primary.Locator.String(), head.String())

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

		if r.Primary.Title != "" {
			o.block("   ", r.Primary.Title)
		}
		if r.Primary.Excerpt != "" {
			o.block("   ", r.Primary.Excerpt)
		}
		if !meta.empty() {
			o.block("   ", meta.String())
		}
		renderMembers(o, r.Members)
		if explained {
			o.block("   ", explain.Render(r.Explanation))
		}
	}
}

// renderMembers shows the cluster's lineage groups. Two members mean two
// independent records; two candidates inside one member mean one record seen
// twice, and collapsing them would turn corroboration into repetition.
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
func renderSourceOutcomes(o *out, reports []recall.SourceReport) {
	o.blank()
	o.line("sources")
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

func renderPlan(o *out, plan recall.Plan) {
	o.blank()
	var head fields
	head.text("profile", plan.Profile)
	head.count("limit", plan.Limit)
	head.number("rank constant", plan.RankConst)
	head.number("corroboration cap", plan.Corrobor)
	head.dur("fusion reserve", plan.Reserve)
	if !plan.Deadline.IsZero() {
		head.text("deadline", stamp(plan.Deadline))
	}
	o.line("plan  " + head.String())

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
// saying what it was.
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
