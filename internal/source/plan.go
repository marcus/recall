package source

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Eligibility reasons. They are a closed vocabulary because they are reported
// to the caller and asserted on by evaluation gates.
const (
	ReasonOutOfScope         = "out_of_scope"
	ReasonDisabled           = "disabled"
	ReasonSensitivity        = "sensitivity_ceiling"
	ReasonUnhealthy          = "unhealthy"
	ReasonDenied             = "denied"
	ReasonAsOfUnsupported    = "as_of_unsupported"
	ReasonRecordTypeMismatch = recall.SkipRecordTypeMismatch
	ReasonBudgetExhausted    = "budget_exhausted"
	ReasonAdapterUnavailable = "adapter_unavailable"
	ReasonStoreConflict      = "store_identity_conflict"

	// The reasons an ADAPTER may state live in internal/recall, beside the
	// outcome they qualify, because they are part of the search contract and
	// not of this planner — an adapter must not have to import the thing that
	// decides its eligibility in order to say what it does not serve. They are
	// re-exported here so [Degrades] reads as one vocabulary.
	ReasonNotApplicable   = recall.SkipNotApplicable
	ReasonUnappliedFilter = recall.SkipFilterUnsupported
	ReasonUnstatedSkip    = recall.SkipUnstated
)

// CanonicalReason accepts an adapter-stated reason, or returns "" for anything
// outside the closed set.
//
// Nothing outside it is passed through. The vocabulary is reported to callers
// and asserted on by evaluation gates, so an adapter that could invent a value
// would be one that could land itself in the non-degrading branch of [Degrades]
// by spelling something new.
func CanonicalReason(reason string) string {
	switch reason {
	case ReasonNotApplicable, ReasonUnappliedFilter,
		ReasonRecordTypeMismatch, ReasonAsOfUnsupported:
		return reason
	default:
		return ""
	}
}

// Applicable reports whether any source was in a position to answer at all.
//
// A source that skipped as not-applicable did not degrade coverage: it is not
// the one that was asked for, and saying otherwise would make every routed
// request look impaired. But when NO source was applicable, the request named a
// boundary nothing crossed, and reporting that as complete coverage claims the
// system looked everywhere and found nothing. It did not look anywhere.
//
// This is why non-applicability is judged over the response and not per source.
// Per source it is routing; over the response it is a filter naming something
// this machine does not have.
func Applicable(reasons []string, searched int) bool {
	if searched > 0 {
		return true
	}
	for _, r := range reasons {
		if r == ReasonNotApplicable {
			return false
		}
	}
	return true
}

// Degrades reports whether an exclusion narrowed what the request could reach.
//
// Not every exclusion is a degradation. A source left out because the user
// scoped the request, disabled it, or set a ceiling above it is the configured
// state of the system working as asked — reporting that as degraded coverage
// would make every well-configured query look impaired, and the signal would
// stop meaning anything.
//
// A source left out because it is unhealthy, denied by its own end, out of
// budget, or unable to honor the request's as_of boundary is different: the
// caller asked for something the system could not fully serve, and that is
// exactly what degraded coverage is for.
// A source that told the core it does not serve what was asked for is in the
// same position as one the user scoped out: it is not the source for this
// request, and calling that degradation would make every routed query look
// impaired. Whether the request reached ANY source is a separate question,
// answered by [Applicable].
func Degrades(reason string) bool {
	switch reason {
	case ReasonOutOfScope, ReasonDisabled, ReasonSensitivity,
		ReasonRecordTypeMismatch, ReasonNotApplicable:
		return false
	default:
		return true
	}
}

// DefaultFusionReserve is time held back from the request deadline so fusion,
// shaping, and rendering are not themselves the thing that blows the budget.
const DefaultFusionReserve = 25 * time.Millisecond

// DefaultPerSourceLimit bounds what one source contributes to the fusion pool,
// so a large corpus cannot flood it.
const DefaultPerSourceLimit = 20

// Target is one eligible source and the budget it was granted.
type Target struct {
	Instance *config.SourceInstance
	Manifest recall.Manifest
	Deadline time.Time
	Limit    int

	// Health is what the source reported when eligibility was decided, carried
	// so that the probe deciding eligibility is the only health probe a request
	// makes of it.
	//
	// Reporting used to call Health a second time after the search, for the
	// generation identity and the cold-start flag it puts in a source report.
	// For a source whose health probe is a network round trip or a process
	// spawn that doubled the cost of every query — the td adapter spent two of
	// its eight spawns per query on it, each reading the whole workspace — and
	// bought nothing: it is the same report, taken later. Later is also the
	// wrong instant to take it. What a report should say is the health that let
	// this source into the plan, not health measured after the answer was
	// already produced from it.
	Health recall.Health
}

// Plan is the resolved retrieval plan: who may answer, with what budget, and
// why anyone was left out.
type Plan struct {
	Profile  string
	Targets  []Target
	Excluded []recall.SourceReport
	Deadline time.Time
	Reserve  time.Duration
	Limit    int
}

// PlanOptions tune plan construction.
type PlanOptions struct {
	Now     func() time.Time
	Reserve time.Duration
	// PerSourceLimit bounds each source's contribution to the pool.
	PerSourceLimit int
}

// BuildPlan decides which sources may answer a request.
//
// Eligibility is hard constraints only: explicit scope, permission, health,
// remaining budget, as_of support, and declared record types. Nothing here
// guesses whether a source is likely to be useful — broad parallel search is
// the default, and a source wrongly excluded is the first entry in the error
// taxonomy for good reason. A source that is merely unlikely competes and
// loses on rank.
func (r *Registry) BuildPlan(ctx context.Context, req recall.QueryRequest, opt PlanOptions) (Plan, error) {
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	reserve := opt.Reserve
	if reserve <= 0 {
		reserve = DefaultFusionReserve
	}
	perSource := opt.PerSourceLimit
	if perSource <= 0 {
		perSource = DefaultPerSourceLimit
	}

	profile, err := r.cfg.ActiveProfile(req.Profile)
	if err != nil {
		return Plan{}, err
	}
	instances, err := r.cfg.ProfileSources(profile.Name)
	if err != nil {
		return Plan{}, err
	}

	start := now()
	plan := Plan{
		Profile:  profile.Name,
		Reserve:  reserve,
		Limit:    req.Limit,
		Deadline: start.Add(time.Duration(req.Budget.LatencyMS) * time.Millisecond),
	}
	if req.Budget.LatencyMS <= 0 {
		// No stated budget is not the same as no budget: an unbounded query
		// would make a hung source indistinguishable from a slow one.
		plan.Deadline = start.Add(DefaultQueryBudget)
	}

	// Every source is handshaken and probed at once, because a handshake and a
	// health probe are both round trips to somewhere else — an index to open, a
	// server to reach, a process to spawn — and running thirteen of them one
	// after another made planning cost the sum of the slowest sources rather
	// than the slowest source. Retrieval itself has always fanned out; this is
	// the same reasoning applied to the step that decides who may answer.
	//
	// Results land in a slice indexed by configured position and are folded
	// back in that order, so the plan a caller reads does not depend on which
	// source answered first. A plan that reordered itself under load would make
	// two identical queries report different things.
	verdicts := make([]verdict, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		if reason, ok := staticIneligible(req, profile, inst); !ok {
			verdicts[i] = verdict{reason: reason}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i] = r.consider(ctx, req, inst, plan.Deadline, reserve, perSource, now)
		}()
	}
	wg.Wait()
	refuseDuplicateStores(instances, verdicts)

	for i, inst := range instances {
		if v := verdicts[i]; v.reason != "" {
			plan.Excluded = append(plan.Excluded, exclude(inst, v.reason, v.diagnostics))
		} else {
			plan.Targets = append(plan.Targets, v.target)
		}
	}
	return plan, nil
}

// refuseDuplicateStores prevents one physical store from entering a plan as
// two independent sources. Different instances can legitimately apply
// different scopes, so silently keeping the first would lose configured
// coverage; refusing every conflicting instance makes the ambiguity explicit
// and prevents duplicated evidence from scoring itself up for corroboration.
//
// The identity comes from adapter health after the store was opened. Location
// strings are deliberately not compared: a repository, subdirectory, symlink,
// and worktree may all name one store, while two directories with the same
// basename may hold separate stores.
func refuseDuplicateStores(instances []*config.SourceInstance, verdicts []verdict) {
	type claim struct {
		adapter  string
		identity string
	}
	claimed := make(map[claim][]int)
	for i, v := range verdicts {
		if v.reason != "" || v.target.Instance == nil {
			continue
		}
		identity, _ := v.target.Health.Diagnostics[protocol.DiagStoreIdentity].(string)
		if identity == "" {
			continue
		}
		key := claim{adapter: instances[i].Adapter, identity: identity}
		claimed[key] = append(claimed[key], i)
	}
	for _, indexes := range claimed {
		if len(indexes) < 2 {
			continue
		}
		ids := make([]string, 0, len(indexes))
		for _, i := range indexes {
			ids = append(ids, instances[i].ID)
		}
		for _, i := range indexes {
			identity, _ := verdicts[i].target.Health.Diagnostics[protocol.DiagStoreIdentity].(string)
			verdicts[i] = verdict{
				reason: ReasonStoreConflict,
				diagnostics: map[string]any{
					protocol.DiagStoreIdentity: identity,
					"conflicting_sources":      ids,
				},
			}
		}
	}
}

// verdict is one source's eligibility: a target, or the reason there is none.
type verdict struct {
	target      Target
	reason      string
	diagnostics map[string]any
}

// consider handshakes one source, probes it, and decides whether it may answer.
//
// The handshake comes before the probe, and the order is load-bearing.
//
// A built-in adapter is constructed unconfigured — it learns its corpus,
// workdir, and settings at the handshake — so probing first asks a source that
// has not been told where to read, and it answers unavailable because that is
// the truth. Every built-in source was excluded as unhealthy on that basis,
// which made `recall query` return nothing while `recall doctor`, which
// initializes before probing, called the same source healthy.
//
// It is safe to handshake here because every permission check has already run
// in [staticIneligible]: initializing a source the ceiling denies would be the
// disclosure the ceiling exists to prevent.
func (r *Registry) consider(
	ctx context.Context,
	req recall.QueryRequest,
	inst *config.SourceInstance,
	budget time.Time,
	reserve time.Duration,
	perSource int,
	now func() time.Time,
) verdict {
	manifest, err := r.Initialize(ctx, inst)
	if err != nil {
		return verdict{reason: ReasonAdapterUnavailable}
	}
	a, err := r.Adapter(inst)
	if err != nil {
		return verdict{reason: ReasonAdapterUnavailable}
	}
	health, err := a.Health(ctx)
	switch {
	case err != nil && health.Status == recall.HealthDenied:
		return verdict{reason: ReasonDenied}
	case err != nil || !health.Usable():
		return verdict{reason: ReasonUnhealthy}
	}
	// A source that cannot honor a historical boundary is excluded and said so.
	// Letting it answer from current state would be a wrong answer wearing the
	// shape of a right one.
	if req.AsOf != nil && !manifest.AsOfSupport.Honors() {
		return verdict{reason: ReasonAsOfUnsupported}
	}
	if !typesOverlap(req, inst, manifest) {
		return verdict{reason: ReasonRecordTypeMismatch}
	}

	deadline := sourceDeadline(now(), budget, reserve, inst.Timeout)
	if !deadline.After(now()) {
		// The budget is already spent. Asking anyway would guarantee a timeout
		// and charge the caller for it.
		return verdict{reason: ReasonBudgetExhausted}
	}
	return verdict{target: Target{
		Instance: inst,
		Manifest: manifest,
		Deadline: deadline,
		Limit:    perSource,
		Health:   health,
	}}
}

// DefaultQueryBudget bounds a request whose caller stated none.
const DefaultQueryBudget = 5 * time.Second

// staticIneligible applies the checks that need nothing from the adapter.
func staticIneligible(req recall.QueryRequest, profile config.Profile, inst *config.SourceInstance) (string, bool) {
	if !inst.Enabled {
		return ReasonDisabled, false
	}
	if req.Scope != nil && len(req.Scope.SourceIDs) > 0 &&
		!slices.Contains(req.Scope.SourceIDs, inst.ID) {
		return ReasonOutOfScope, false
	}
	// The ceiling is a permission boundary, so it is checked before anything is
	// asked of the source at all — not applied to results afterwards.
	if !profile.Permits(*inst) {
		return ReasonSensitivity, false
	}
	return "", true
}

// typesOverlap reports whether the source can hold any record type the request
// asked for. An unscoped request matches everything.
func typesOverlap(req recall.QueryRequest, inst *config.SourceInstance, m recall.Manifest) bool {
	if req.Scope == nil || len(req.Scope.RecordTypes) == 0 {
		return true
	}
	available := inst.RecordTypes
	if len(available) == 0 {
		available = m.RecordTypes
	}
	if len(available) == 0 {
		// A source that declares nothing might hold anything, and excluding it
		// would be a guess.
		return true
	}
	for _, want := range req.Scope.RecordTypes {
		if slices.Contains(available, want) {
			return true
		}
	}
	return false
}

// sourceDeadline is the earlier of the source's own timeout and what remains of
// the request budget once fusion's reserve is held back.
func sourceDeadline(now, deadline time.Time, reserve, timeout time.Duration) time.Time {
	usable := deadline.Add(-reserve)
	if timeout > 0 {
		if own := now.Add(timeout); own.Before(usable) {
			return own
		}
	}
	return usable
}

func exclude(inst *config.SourceInstance, reason string, diagnostics map[string]any) recall.SourceReport {
	return recall.SourceReport{
		SourceUID:   inst.UID,
		SourceID:    inst.ID,
		Outcome:     recall.SearchSkipped,
		Reason:      reason,
		Diagnostics: diagnostics,
	}
}

// AsPlan renders the plan into the response envelope, so a caller can see what
// was searched rather than inferring it from what came back.
func (p Plan) AsPlan(rankConst, corroborationCap float64) recall.Plan {
	out := recall.Plan{
		Profile:   p.Profile,
		Deadline:  p.Deadline,
		Reserve:   p.Reserve,
		Limit:     p.Limit,
		RankConst: rankConst,
		Corrobor:  corroborationCap,
	}
	for _, t := range p.Targets {
		out.Sources = append(out.Sources, recall.PlanSource{
			SourceUID: t.Instance.UID,
			SourceID:  t.Instance.ID,
			Eligible:  true,
			Limit:     t.Limit,
			Timeout:   time.Until(t.Deadline),
			Prior:     t.Instance.BasePrior,
		})
	}
	for _, e := range p.Excluded {
		out.Sources = append(out.Sources, recall.PlanSource{
			SourceUID:   e.SourceUID,
			SourceID:    e.SourceID,
			Eligible:    false,
			Reason:      e.Reason,
			Diagnostics: e.Diagnostics,
		})
	}
	return out
}
