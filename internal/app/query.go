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

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/recall"
	"github.com/marcus/recall/internal/source"
)

// App is the application core.
type App struct {
	cfg      *config.Config
	registry Registry
	ranker   *ranking.Ranker
	shaper   evidence.Shaper
	limits   evidence.Limits

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
	Now      func() time.Time
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

	results := a.fanOut(ctx, req, plan)

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
		Limit:             req.Limit,
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

	shaped := a.shaper.Shape(fusion.Results, req.Budget)
	outcomes := reports(plan, results)

	resp := recall.QueryResponse{
		Results:        shaped.Results,
		SourceOutcomes: outcomes,
		Plan:           plan.AsPlan(a.ranker.Config().RankConstant, a.ranker.Config().CorroborationCap),
		Suppressed:     append(suppressed, fusion.Suppressed...),
		Coverage:       coverage(outcomes),
		Truncated:      shaped.Truncated || fusion.Truncated,
		DroppedResults: shaped.Dropped + fusion.Dropped,
		Elapsed:        a.now().Sub(start),
	}
	resp.Outcome = outcome(resp, outcomes)
	return resp, nil
}

// searchResult is one source's answer plus the reporting that goes with it.
type searchResult struct {
	target   source.Target
	response recall.SearchResponse
	elapsed  time.Duration
	health   recall.Health
	err      error
}

// fanOut asks every eligible source concurrently, each under its own deadline.
func (a *App) fanOut(ctx context.Context, req recall.QueryRequest, plan source.Plan) []searchResult {
	out := make([]searchResult, len(plan.Targets))
	var wg sync.WaitGroup

	for i, target := range plan.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = a.searchOne(ctx, req, target)
		}()
	}
	wg.Wait()
	return out
}

func (a *App) searchOne(ctx context.Context, req recall.QueryRequest, t source.Target) searchResult {
	res := searchResult{target: t}
	started := a.now()

	adp, err := a.registry.Adapter(t.Instance)
	if err != nil {
		res.err = err
		res.response = recall.SearchResponse{Outcome: recall.SearchUnavailable}
		return res
	}

	ctx, cancel := context.WithDeadline(ctx, t.Deadline)
	defer cancel()

	sr := recall.SearchRequest{
		Query:    req.Query,
		AsOf:     req.AsOf,
		Limit:    t.Limit,
		Deadline: t.Deadline,
	}
	if req.Scope != nil {
		sr.Filters = recall.Filters{
			RecordTypes: req.Scope.RecordTypes,
			Since:       req.Scope.Since,
			Until:       req.Scope.Until,
		}
	}
	if t.Manifest.Can(recall.CapContextExpansion) {
		sr.Context = req.Context
	}

	res.response, res.err = adp.Search(ctx, sr)
	res.elapsed = a.now().Sub(started)

	// A source that could not answer never reports success with no candidates.
	if res.err != nil && !res.response.Outcome.Degrades() {
		res.response.Outcome = recall.SearchFailed
	}
	if h, herr := adp.Health(ctx); herr == nil {
		res.health = h
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
// Freshness is a source-health fact, not a ranking one, so fusion leaves it
// empty. Without this the freshness block would be missing from every
// explanation, and invariant 6 would fail for exactly the fields that say
// whether an answer is current.
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
		e.Freshness.ObservedAt = results[i].Primary.ObservedAt
		e.Freshness.ConfirmedAt = results[i].Primary.ConfirmedAt
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
		}
		if r.err != nil && rep.Reason == "" {
			rep.Reason = classify(r.err)
		}
		out = append(out, rep)
	}
	return append(out, plan.Excluded...)
}

// coverage is complete when every source that could have answered did.
//
// A source excluded by the user's own policy — scope, ceiling, disabled — does
// not degrade anything: that is the system doing what it was configured to do.
// A source that was eligible and could not answer does.
func coverage(reports []recall.SourceReport) recall.Coverage {
	for _, r := range reports {
		degraded := r.Outcome.Degrades()
		if r.Outcome == recall.SearchSkipped {
			degraded = source.Degrades(r.Reason)
		}
		if degraded {
			return recall.CoverageDegraded
		}
	}
	return recall.CoverageComplete
}

// outcome decides what Recall did, independently of how much it could see.
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

var _ lineage.Resolver = (*config.Config)(nil)
