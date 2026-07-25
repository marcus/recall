package source

import (
	"context"
	"slices"
	"time"

	"github.com/marcus/recall/internal/config"
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
	ReasonRecordTypeMismatch = "record_type_mismatch"
	ReasonBudgetExhausted    = "budget_exhausted"
	ReasonAdapterUnavailable = "adapter_unavailable"
)

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
func Degrades(reason string) bool {
	switch reason {
	case ReasonOutOfScope, ReasonDisabled, ReasonSensitivity, ReasonRecordTypeMismatch:
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

	for _, inst := range instances {
		if reason, ok := staticIneligible(req, profile, inst); !ok {
			plan.Excluded = append(plan.Excluded, exclude(inst, reason))
			continue
		}

		// Health is the last static check because it is the only one that can
		// cost a process spawn.
		a, err := r.Adapter(inst)
		if err != nil {
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonAdapterUnavailable))
			continue
		}
		health, err := a.Health(ctx)
		switch {
		case err != nil && health.Status == recall.HealthDenied:
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonDenied))
			continue
		case err != nil || !health.Usable():
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonUnhealthy))
			continue
		}

		manifest, err := r.Initialize(ctx, inst)
		if err != nil {
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonAdapterUnavailable))
			continue
		}
		// A source that cannot honor a historical boundary is excluded and said
		// so. Letting it answer from current state would be a wrong answer
		// wearing the shape of a right one.
		if req.AsOf != nil && !manifest.AsOfSupport.Honors() {
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonAsOfUnsupported))
			continue
		}
		if !typesOverlap(req, inst, manifest) {
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonRecordTypeMismatch))
			continue
		}

		deadline := sourceDeadline(now(), plan.Deadline, reserve, inst.Timeout)
		if !deadline.After(now()) {
			// The budget is already spent. Asking anyway would guarantee a
			// timeout and charge the caller for it.
			plan.Excluded = append(plan.Excluded, exclude(inst, ReasonBudgetExhausted))
			continue
		}
		plan.Targets = append(plan.Targets, Target{
			Instance: inst,
			Manifest: manifest,
			Deadline: deadline,
			Limit:    perSource,
		})
	}
	return plan, nil
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

func exclude(inst *config.SourceInstance, reason string) recall.SourceReport {
	return recall.SourceReport{
		SourceUID: inst.UID,
		SourceID:  inst.ID,
		Outcome:   recall.SearchSkipped,
		Reason:    reason,
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
			SourceUID: e.SourceUID,
			SourceID:  e.SourceID,
			Eligible:  false,
			Reason:    e.Reason,
		})
	}
	return out
}
