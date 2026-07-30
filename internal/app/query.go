// Package app is the application layer every product surface calls.
//
// Boundary: it owns orchestration — plan, fan out, sanitize, fuse, shape,
// decide the outcome — and owns no retrieval, ranking, or storage logic of its
// own. The CLI, the HTTP API, and the MCP server are transports over this, and
// none of them may acquire ranking, permission, or expansion behavior a
// different transport would not have.
package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// App is the application core.
type App struct {
	cfg      *config.Config
	registry Registry
	ranker   *ranking.Ranker
	limits   evidence.Limits

	// costs prices a response for the surface that will render it. A surface
	// with no entry here is priced as the serialized response, which is the
	// largest rendering and therefore never lets a projection of it overrun.
	costs map[recall.ResponseSurface]evidence.Cost

	// now is injectable so evaluation runs can pin the clock to a case's as_of.
	now func() time.Time
}

// Registry is the subset of the source registry the app needs. Narrowing it
// keeps the app testable without a configuration tree on disk.
type Registry interface {
	BuildPlan(ctx context.Context, req recall.QueryRequest, opt source.PlanOptions) (source.Plan, error)
	Adapter(inst *config.SourceInstance) (adapter.Adapter, error)
	Initialize(ctx context.Context, inst *config.SourceInstance) (recall.Manifest, error)
}

// Options configure an App.
type Options struct {
	Config   *config.Config
	Registry Registry
	Ranker   *ranking.Ranker
	Limits   evidence.Limits

	// Costs prices a response per surface. Only a transport knows what its own
	// rendering costs, so the transport supplies it rather than the core
	// keeping a table of numbers it cannot verify.
	Costs map[recall.ResponseSurface]evidence.Cost

	Now func() time.Time
}

// New builds the application core.
func New(opt Options) *App {
	limits := opt.Limits
	if limits == (evidence.Limits{}) {
		limits = evidence.DefaultLimits()
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	return &App{
		cfg:      opt.Config,
		registry: opt.Registry,
		ranker:   opt.Ranker,
		limits:   limits,
		costs:    opt.Costs,
		now:      now,
	}
}

// Query searches every eligible source and fuses what comes back.
func (a *App) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	start := a.now()

	plan, err := a.registry.BuildPlan(ctx, req, source.PlanOptions{Now: a.now})
	if err != nil {
		return recall.QueryResponse{
			Outcome:  recall.OutcomeFailed,
			Coverage: recall.CoverageDegraded,
		}, err
	}

	results := a.fanOut(ctx, plan)

	// Sanitize and enforce the ceiling before anything is ranked. Both are
	// boundary duties: a candidate that may not be shown must not influence
	// what is, and untrusted text must not reach a terminal whatever happens
	// to it downstream.
	profile, _ := a.cfg.ActiveProfile(plan.Profile)
	pool, suppressed := a.admit(results, profile.MaxSensitivity)

	resolver := a.cfg
	fusion, err := a.ranker.Fuse(ranking.Request{
		Candidates:        pool,
		Resolver:          resolver,
		SourceDerivations: a.derivations(plan),
		SourceLocations:   sourceLocations(plan),
		Limit:             req.Limit,
		QueryClass:        ranking.ClassifyQuery(req.Query),
		StableIdentifiers: ranking.StableIdentifiers(req.Query),
		Mode:              req.Mode,
		SuppressLineages:  req.SuppressLineages,
	})
	if err != nil {
		return recall.QueryResponse{
			Outcome:  recall.OutcomeFailed,
			Coverage: recall.CoverageDegraded,
		}, err
	}

	a.annotate(fusion.Results, plan, results)

	outcomes := reports(plan, results)

	resp := recall.QueryResponse{
		Results:        fusion.Results,
		SourceOutcomes: outcomes,
		// Carried on every response and kept only by shaping, which is the one
		// thing that knows whether the ledger it stands in for was affordable.
		SourceSummary:  summarize(outcomes),
		Plan:           plan.AsPlan(a.rules(fusion)),
		Suppressed:     append(suppressed, fusion.Suppressed...),
		Coverage:       coverage(outcomes),
		Truncated:      fusion.Truncated,
		DroppedResults: fusion.Dropped,
		Elapsed:        a.now().Sub(start),
	}
	// Decided before shaping, on what the corpus returned. A budget too small
	// for one result is a fact about the request, and reading it back as
	// "nothing matched" would make the caller's own budget into a claim about
	// the corpus.
	resp.Outcome = outcome(resp, outcomes)

	// Shaped last, and against the frame above: the outcome line, the source
	// ledger, and the plan are part of what gets rendered, so results are fitted
	// into what remains after them rather than on top of them — and when even
	// that does not fit, the diagnostics summarize rather than the budget being
	// waived.
	return evidence.Shape(resp, req.Budget, a.cost(req.Budget.Surface)).Response, nil
}

// summarize reduces the per-source ledger to what stands in for it when the
// response budget cannot afford the whole thing: how many sources reported
// each outcome, and which of them could not answer.
//
// The degraded list is the part that must survive. Everything else here is a
// convenience; that list is a claim about the evidence, and a summary that
// dropped it would turn an incomplete answer into a silent one.
func summarize(reports []recall.SourceReport) *recall.SourceSummary {
	if len(reports) == 0 {
		return nil
	}
	out := recall.SourceSummary{
		Sources:  len(reports),
		Outcomes: make(map[recall.SearchOutcome]int, 4),
		Degraded: source.DegradedReports(reports),
	}
	for _, r := range reports {
		out.Outcomes[r.Outcome]++
	}
	return &out
}

// rules is the fusion configuration this response reports back. The limit comes
// from the fusion that ran rather than from the configuration, because a request
// may override it and the plan should state what applied to this caller.
func (a *App) rules(fusion ranking.Fusion) recall.FusionRules {
	cfg := a.ranker.Config()
	return recall.FusionRules{
		RankConstant:     cfg.RankConstant,
		CorroborationCap: cfg.CorroborationCap,
		RelevanceFloor:   cfg.RelevanceFloor,
		Limit:            fusion.Limit,
	}
}

// cost prices a response for the surface that asked for it.
func (a *App) cost(surface recall.ResponseSurface) evidence.Cost {
	if c, ok := a.costs[surface]; ok {
		return c
	}
	return evidence.StructuredCost{}
}

// searchResult is one source's answer plus the reporting that goes with it.
type searchResult struct {
	target   source.Target
	response recall.SearchResponse
	elapsed  time.Duration
	health   recall.Health
	err      error
	timeout  *recall.TimeoutDetail
}

// fanOut asks every eligible source concurrently, each under its own deadline.
func (a *App) fanOut(ctx context.Context, plan source.Plan) []searchResult {
	out := make([]searchResult, len(plan.Targets))
	var wg sync.WaitGroup

	for i, target := range plan.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = a.searchOne(ctx, target)
		}()
	}
	wg.Wait()
	return out
}

func (a *App) searchOne(ctx context.Context, t source.Target) searchResult {
	// The health carried on the target is the probe that decided this source
	// was eligible, and it is the only one this request makes. Probing again
	// after the search cost a second round trip per source — for the td adapter
	// a second process spawn reading the whole workspace — to restate a report
	// the plan already had. See the field's own comment in source.Target.
	res := searchResult{target: t, health: t.Health}
	started := a.now()

	adp, err := a.registry.Adapter(t.Instance)
	if err != nil {
		res.err = err
		res.response = recall.SearchResponse{Outcome: recall.SearchUnavailable}
		return res
	}

	ctx, cancel := context.WithDeadline(ctx, t.Deadline)
	defer cancel()

	if prepared, ok := adp.(adapter.PreparedSearcher); ok {
		res.response, res.err = prepared.SearchPrepared(ctx, t.Request, t.Preparation)
		res.elapsed = t.Preparation.Elapsed
	} else {
		res.response, res.err = adp.Search(ctx, t.Request)
		res.elapsed = a.now().Sub(started)
	}
	if res.response.Outcome == recall.SearchTimeout || timeoutError(res.err) {
		if ctx.Err() == nil {
			res.timeout = &recall.TimeoutDetail{Budget: recall.TimeoutAdapterInternal}
		} else {
			deadline := t.Deadline
			res.timeout = &recall.TimeoutDetail{
				Budget: t.TimeoutBudget, Limit: t.TimeoutLimit, Deadline: &deadline,
			}
		}
	}
	if res.response.Outcome == recall.SearchSkipped {
		// Skipping means the adapter did not answer this constrained question.
		// Discard candidates even if a broken external adapter sent them.
		res.response.Candidates = []recall.Candidate{}
	}

	// A source that could not answer never reports success with no candidates.
	if res.err != nil && !res.response.Outcome.Degrades() {
		res.response.Outcome = recall.SearchFailed
	}
	return res
}

// admit sanitizes candidates and drops anything above the profile ceiling.
//
// The ceiling is applied to candidates as well as to sources because an adapter
// may classify a record more restrictively than the source it came from, and a
// record raised above the ceiling must not be shown even though its source was
// eligible.
func (a *App) admit(results []searchResult, ceiling recall.Sensitivity) ([]recall.Candidate, []recall.Suppression) {
	var pool []recall.Candidate
	denied := 0

	for _, r := range results {
		if r.response.Outcome == recall.SearchSkipped {
			// A skipped source did not answer this question. Even a malformed
			// or adversarial adapter response cannot smuggle broader evidence
			// into fusion alongside the skip.
			continue
		}
		for _, c := range r.response.Candidates {
			if !evidence.Permit(c, ceiling) {
				denied++
				continue
			}
			clean, _ := evidence.Sanitize(c, a.limits)
			pool = append(pool, clean)
		}
	}
	if denied == 0 {
		return pool, nil
	}
	// Counted, not named: the count tells a host something was withheld
	// without telling it what.
	return pool, []recall.Suppression{{Reason: recall.SuppressSensitivity, Count: denied}}
}

// derivations collects manifest-declared whole-source projections.
// sourceLocations is the resolved location each planned source opened, keyed by
// source identity.
//
// It exists so that fusion can refuse to let two sources over ONE store
// corroborate each other for holding the same record. A location is the one
// thing about a source that the source itself cannot establish — an adapter
// names its own store at most, and two adapters' spellings of a path are not
// comparable — so it comes from here, where configuration has already resolved
// it, rather than from anything a candidate carries.
//
// The resolved value, not the declared one: a relative path and the absolute
// path it resolves to are one directory, and the declared spellings of the two
// sources need not agree for the store to be the same.
//
// Sources with no location are simply absent from the map. Every structured
// source is one, so a profile of them behaves exactly as it did before this
// existed.
func sourceLocations(plan source.Plan) map[recall.SourceUID]string {
	out := make(map[recall.SourceUID]string, len(plan.Targets))
	for _, t := range plan.Targets {
		if location := t.Instance.Location; location != "" {
			out[t.Instance.UID] = location
		}
	}
	return out
}

func (a *App) derivations(plan source.Plan) map[recall.SourceUID]recall.SourceUID {
	out := map[recall.SourceUID]recall.SourceUID{}
	for _, t := range plan.Targets {
		if t.Manifest.DerivesFrom == "" {
			continue
		}
		// A manifest names the upstream source by display name; only
		// configuration can turn that into an identity.
		if uid, ok := a.cfg.UID(t.Manifest.DerivesFrom); ok {
			out[t.Instance.UID] = uid
		}
	}
	return out
}

// annotate fills the explanation fields ranking cannot know.
//
// Health-level freshness is an application fact, not a ranking one, so fusion
// cannot fill the mode or index identity. Candidate-level timestamps already
// belong to the score basis selected by ranking and stay untouched here: a
// document may display a different matched chunk, and its timestamps must not
// be reattributed as the evidence that earned the score.
func (a *App) annotate(results []recall.Result, plan source.Plan, searches []searchResult) {
	byUID := map[recall.SourceUID]searchResult{}
	for _, s := range searches {
		byUID[s.target.Instance.UID] = s
	}
	for i := range results {
		e := &results[i].Explanation
		s, ok := byUID[e.SourceUID]
		if !ok {
			continue
		}
		e.Freshness.Mode = s.target.Instance.FreshnessMode
		e.Freshness.SourceRevision = s.response.SourceWatermark
		e.Freshness.IndexGeneration = s.health.IndexGeneration
		e.Freshness.IndexModel = s.health.IndexModel
		e.Freshness.IndexConfig = s.health.IndexConfig
		e.Freshness.AsOfHonored = s.target.Manifest.AsOfSupport
	}
	_ = plan
}

// reports renders every source's outcome, eligible or not.
func reports(plan source.Plan, results []searchResult) []recall.SourceReport {
	out := make([]recall.SourceReport, 0, len(results)+len(plan.Excluded))
	for _, r := range results {
		rep := recall.SourceReport{
			SourceUID:       r.target.Instance.UID,
			SourceID:        r.target.Instance.ID,
			Outcome:         r.response.Outcome,
			Candidates:      len(r.response.Candidates),
			Elapsed:         r.elapsed,
			ColdStart:       r.health.ColdStart,
			SourceWatermark: r.response.SourceWatermark,
			IndexGeneration: r.health.IndexGeneration,
			ConfirmedAt:     r.health.LastSuccess,
			Diagnostics:     r.response.Diagnostics,
			Reason:          skipReason(r.response),
		}
		if r.err != nil && rep.Reason == "" {
			rep.Reason = classify(r.err)
		}
		rep.Timeout = r.timeout
		out = append(out, rep)
	}
	return append(out, plan.Excluded...)
}

// skipReason reads an adapter's stated reason for skipping, in the closed
// vocabulary [source.Degrades] judges.
//
// It is only read for [recall.SearchSkipped], and an adapter that skips without
// naming a reason gets one that degrades. That default is the safe direction:
// the alternative reads a silent skip as a boundary the request was free to
// miss, which is the assumption this whole change exists to remove.
func skipReason(resp recall.SearchResponse) string {
	if resp.Outcome != recall.SearchSkipped {
		return ""
	}
	if reason := source.CanonicalReason(resp.Reason); reason != "" {
		return reason
	}
	return source.ReasonUnstatedSkip
}

// coverage is complete when every source that could have answered did.
//
// A source excluded by the user's own policy — scope, ceiling, disabled — does
// not degrade anything: that is the system doing what it was configured to do.
// A source that was eligible and could not answer does.
func coverage(reports []recall.SourceReport) recall.Coverage {
	reasons := make([]string, 0, len(reports))
	searched := 0
	for _, r := range reports {
		degraded := r.Outcome.Degrades()
		if r.Outcome == recall.SearchSkipped {
			degraded = source.Degrades(r.Reason)
			reasons = append(reasons, r.Reason)
		} else {
			searched++
		}
		if degraded {
			return recall.CoverageDegraded
		}
	}
	// Every source said the request did not apply to it, so nothing looked.
	// Per source that is routing and does not degrade; over the whole response
	// it means the request named a boundary this machine does not have, and
	// `complete` would claim the system searched everywhere for it.
	if !source.Applicable(reasons, searched) {
		return recall.CoverageDegraded
	}
	return recall.CoverageComplete
}

// outcome decides what Recall did, independently of how much it could see and
// of how much of it fit — resp carries the fused results, before shaping.
//
// Abstention is a rule over results and source outcomes, never a threshold on a
// fusion score: those scores are ordinal and uncalibrated, so a threshold on
// one would be a number pretending to be a confidence.
func outcome(resp recall.QueryResponse, reports []recall.SourceReport) recall.Outcome {
	if len(resp.Results) > 0 {
		return recall.OutcomeAnswered
	}
	asked, answered := 0, 0
	for _, r := range reports {
		if r.Outcome == recall.SearchSkipped {
			continue
		}
		asked++
		if r.Outcome.Searched() {
			answered++
		}
	}
	// Nothing was found, but that is only an answer if something looked. If
	// every source that was asked failed, "no results" would be a claim the
	// evidence does not support.
	if asked > 0 && answered == 0 {
		return recall.OutcomeFailed
	}
	return recall.OutcomeAbstained
}

// classify names a source failure in the closed vocabulary a SourceReport uses,
// so a caller can act on it without reading an adapter's prose — and so an
// adapter's own error text, which is source-influenced, never reaches a report.
func classify(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, protocol.ErrSourceDenied):
		return "denied"
	case errors.Is(err, protocol.ErrSourceUnavailable):
		return "unreachable"
	case errors.Is(err, protocol.ErrAsOfUnsupported):
		return source.ReasonAsOfUnsupported
	case errors.Is(err, protocol.ErrBudgetExceeded):
		return "budget_exceeded"
	default:
		return "failed"
	}
}

func timeoutError(err error) bool {
	if err == nil {
		return false
	}
	var call *protocol.CallTimeout
	return errors.As(err, &call) ||
		errors.Is(err, protocol.ErrDeadlineExceeded) ||
		errors.Is(err, context.DeadlineExceeded)
}

var _ lineage.Resolver = (*config.Config)(nil)
