package source

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Eligibility reasons. They are a closed vocabulary because they are reported
// to the caller and asserted on by evaluation gates.
const (
	ReasonOutOfScope         = "out_of_scope"
	ReasonOutOfProfile       = "out_of_profile"
	ReasonDisabled           = "disabled"
	ReasonSensitivity        = "sensitivity_ceiling"
	ReasonUnhealthy          = "unhealthy"
	ReasonDenied             = "denied"
	ReasonAsOfUnsupported    = "as_of_unsupported"
	ReasonRecordTypeMismatch = recall.SkipRecordTypeMismatch
	ReasonBudgetExhausted    = "budget_exhausted"
	ReasonAdapterUnavailable = "adapter_unavailable"
	ReasonStoreConflict      = "store_identity_conflict"

	// The reasons an ADAPTER may state live in pkg/recall, beside the
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

// DegradedReports names every source that was eligible and could not answer,
// as "source_id (reason)".
//
// One implementation because it is one rule. Every surface states this list —
// the CLI unflagged, the MCP text block, the summary that stands in for the
// ledger when a budget cannot afford it — and a second opinion about which
// outcomes degrade would let one surface fall silent about a source another one
// names.
func DegradedReports(reports []recall.SourceReport) []string {
	var out []string
	for _, r := range reports {
		degrades := r.Outcome.Degrades()
		if r.Outcome == recall.SearchSkipped {
			degrades = Degrades(r.Reason)
		}
		if !degrades {
			continue
		}
		reason := r.Reason
		if reason == "" {
			reason = string(r.Outcome)
		}
		if r.Timeout != nil {
			switch r.Timeout.Budget {
			case recall.TimeoutRequestLatency:
				reason += fmt.Sprintf(": request latency budget %s", r.Timeout.Limit)
			case recall.TimeoutSourceLimit:
				reason += fmt.Sprintf(": source timeout %s", r.Timeout.Limit)
			case recall.TimeoutCallerDeadline:
				reason += ": caller deadline"
			case recall.TimeoutAdapterInternal:
				reason += ": adapter-internal deadline"
			}
		}
		out = append(out, r.SourceID+" ("+reason+")")
	}
	return out
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
	// TimeoutBudget names the configured budget that supplied Deadline.
	TimeoutBudget recall.TimeoutBudget
	// TimeoutLimit is that budget's configured duration before the fusion
	// reserve is applied.
	TimeoutLimit time.Duration
	Limit        int

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

	// Request is the exact adapter request prepared while this target's
	// deadline and eligibility were decided.
	Request recall.SearchRequest

	// Preparation is adapter-owned, request-scoped state produced beside
	// Health. The core carries it from eligibility to Search without reading,
	// serializing, or retaining it beyond this plan.
	Preparation adapter.SearchPreparation
}

// Plan is the resolved retrieval plan: who may answer, with what budget, and
// why anyone was left out.
type Plan struct {
	Profile  string
	Targets  []Target
	Excluded []recall.SourceReport

	// ExcludedRelevanceBases preserves declarations from sources that
	// initialized successfully and were excluded by a later eligibility check.
	// A source excluded before its handshake has no entry: permission and
	// static routing boundaries must not start an adapter merely to decorate a
	// plan.
	ExcludedRelevanceBases map[recall.SourceUID]recall.RelevanceBasis

	Deadline time.Time
	Reserve  time.Duration
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

	outside, err := r.scopedOutOfProfile(req, profile, instances)
	if err != nil {
		return Plan{}, err
	}

	start := now()
	queryBudget := time.Duration(req.Budget.LatencyMS) * time.Millisecond
	if queryBudget <= 0 {
		queryBudget = DefaultQueryBudget
	}
	plan := Plan{
		Profile:  profile.Name,
		Reserve:  reserve,
		Deadline: start.Add(queryBudget),
		Excluded: outside,
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
			verdicts[i] = r.consider(
				ctx, req, inst, plan.Deadline, queryBudget, reserve, perSource, now)
		}()
	}
	wg.Wait()
	refuseDuplicateStores(instances, verdicts)

	for i, inst := range instances {
		if v := verdicts[i]; v.reason != "" {
			plan.Excluded = append(plan.Excluded, exclude(inst, v.reason, v.diagnostics))
			if v.relevanceBasis != "" {
				if plan.ExcludedRelevanceBases == nil {
					plan.ExcludedRelevanceBases = make(map[recall.SourceUID]recall.RelevanceBasis)
				}
				plan.ExcludedRelevanceBases[inst.UID] = v.relevanceBasis
			}
		} else {
			plan.Targets = append(plan.Targets, v.target)
		}
	}
	return plan, nil
}

// scopedOutOfProfile answers what a `scope source=` naming something the
// profile does not contain means.
//
// A source id is a thing the caller can see in `recall sources` and reasonably
// expects to be askable, so naming one is a request that either narrows the
// profile or cannot be satisfied as written. There is no third reading in which
// it narrows the profile to nothing and the empty answer is a fact about the
// corpus:
//
//   - every named source outside the profile is refused outright, because the
//     alternative is `outcome abstained  coverage complete  elapsed 0s`, which
//     says every eligible source answered and none knows. Nothing was asked.
//     The message names a profile that does contain the source, which
//     `recall sources` already knows.
//   - a partial overlap answers from the sources that are in the profile and
//     reports the rest as excluded, which degrades coverage. The caller asked
//     for evidence from a set this profile cannot fully reach, and that is
//     exactly what degraded means.
//
// Only `scope source=` is treated this way. A type, project, or entity that
// matches nothing is a true absence — the caller named a property of records,
// not a source that exists and was not asked.
func (r *Registry) scopedOutOfProfile(
	req recall.QueryRequest,
	profile config.Profile,
	instances []*config.SourceInstance,
) ([]recall.SourceReport, error) {
	if req.Scope == nil || len(req.Scope.SourceIDs) == 0 {
		return nil, nil
	}
	member := make(map[string]bool, len(instances))
	for _, inst := range instances {
		member[inst.ID] = true
	}

	var (
		reports   []recall.SourceReport
		inside    int
		elsewhere []string
		unknown   []string
	)
	// A source named twice is one source. `--scope source=a --scope source=a`
	// is a caller repeating itself, not two exclusions, and reporting it twice
	// would put the same name in the degraded-coverage line twice.
	seen := make(map[string]bool, len(req.Scope.SourceIDs))
	for _, id := range req.Scope.SourceIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if member[id] {
			inside++
			continue
		}
		inst, configured := r.cfg.Source(id)
		if !configured {
			unknown = append(unknown, id)
			continue
		}
		elsewhere = append(elsewhere, id)
		reports = append(reports, recall.SourceReport{
			SourceUID: inst.UID,
			SourceID:  inst.ID,
			Outcome:   recall.SearchSkipped,
			Reason:    ReasonOutOfProfile,
			Diagnostics: map[string]any{
				"profile":  profile.Name,
				"profiles": r.cfg.ProfilesContaining(id),
			},
		})
	}
	if len(unknown) > 0 {
		// An id no source answers to is refused however much else was named.
		// There is nothing to put in the ledger for it — no uid, no instance,
		// nothing this installation could report on — so answering the rest
		// would report `coverage: complete` over a request that named
		// something this machine does not have, which is the same false
		// completeness as the wholly-out-of-profile case. It is also almost
		// always a typo, and a typo the caller cannot see is one they will
		// trust.
		return nil, unsatisfiableScope(r.cfg, profile.Name, elsewhere, unknown)
	}
	if inside > 0 {
		// Some of what was named is here, and everything named exists. Answer
		// from the members, and let the rest degrade coverage rather than
		// disappear.
		return reports, nil
	}
	return nil, unsatisfiableScope(r.cfg, profile.Name, elsewhere, nil)
}

// unsatisfiableScope writes the refusal, naming where the source can be asked.
func unsatisfiableScope(cfg *config.Config, profile string, elsewhere, unknown []string) error {
	named := append(append([]string(nil), elsewhere...), unknown...)
	slices.Sort(named)
	switch {
	case len(elsewhere) == 0:
		return fmt.Errorf("%w: scope source=%s: no such source is configured; `recall sources` lists what this installation has",
			recall.ErrUnsatisfiableScope, strings.Join(unknown, ","))
	case len(unknown) > 0:
		return fmt.Errorf("%w: scope source=%s: no such source is configured, and %s is not in profile %q; `recall sources` lists what this installation has",
			recall.ErrUnsatisfiableScope, strings.Join(unknown, ","), strings.Join(elsewhere, ","), profile)
	default:
		var advice []string
		for _, id := range elsewhere {
			if profiles := cfg.ProfilesContaining(id); len(profiles) > 0 {
				advice = append(advice, fmt.Sprintf("%s is in %s", id, strings.Join(profiles, ", ")))
			} else {
				advice = append(advice, fmt.Sprintf("%s is in no profile", id))
			}
		}
		return fmt.Errorf("%w: scope source=%s: not in profile %q, so nothing would be asked; %s. Use --profile, or widen the scope",
			recall.ErrUnsatisfiableScope, strings.Join(named, ","), profile, strings.Join(advice, "; "))
	}
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
				reason:         ReasonStoreConflict,
				relevanceBasis: verdicts[i].relevanceBasis,
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
	target         Target
	reason         string
	relevanceBasis recall.RelevanceBasis
	diagnostics    map[string]any
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
	queryBudget time.Duration,
	reserve time.Duration,
	perSource int,
	now func() time.Time,
) verdict {
	manifest, err := r.Initialize(ctx, inst)
	if err != nil {
		return verdict{reason: ReasonAdapterUnavailable}
	}
	excludeAfterHandshake := func(reason string) verdict {
		return verdict{reason: reason, relevanceBasis: manifest.RelevanceBasis}
	}
	a, err := r.Adapter(inst)
	if err != nil {
		return excludeAfterHandshake(ReasonAdapterUnavailable)
	}
	deadline, timeoutBudget, timeoutLimit := sourceDeadline(
		now(), budget, queryBudget, reserve, inst.Timeout)
	searchReq := recall.SearchRequest{
		Query:    req.Query,
		AsOf:     req.AsOf,
		Limit:    perSource,
		Deadline: deadline,
	}
	if req.Scope != nil {
		searchReq.Filters = recall.Filters{
			RecordTypes: req.Scope.RecordTypes,
			Entities:    req.Scope.Entities,
			Since:       req.Scope.Since,
			Until:       req.Scope.Until,
			Project:     req.Scope.Project,
		}
	}
	if manifest.Can(recall.CapContextExpansion) {
		searchReq.Context = req.Context
	}
	var health recall.Health
	var preparation adapter.SearchPreparation
	canSearchCurrent := req.AsOf == nil || manifest.AsOfSupport.Honors()
	canSearchTypes := typesOverlap(req, inst, manifest)
	if prepared, ok := a.(adapter.PreparedSearcher); ok &&
		deadline.After(now()) && canSearchCurrent && canSearchTypes {
		health, preparation, err = prepared.PrepareSearch(ctx, searchReq)
	} else {
		health, err = a.Health(ctx)
	}
	switch {
	case err != nil && health.Status == recall.HealthDenied:
		return excludeAfterHandshake(ReasonDenied)
	case err != nil || !health.Usable():
		return excludeAfterHandshake(ReasonUnhealthy)
	}
	// A source that cannot honor a historical boundary is excluded and said so.
	// Letting it answer from current state would be a wrong answer wearing the
	// shape of a right one.
	if !canSearchCurrent {
		return excludeAfterHandshake(ReasonAsOfUnsupported)
	}
	if !canSearchTypes {
		return excludeAfterHandshake(ReasonRecordTypeMismatch)
	}

	if !deadline.After(now()) {
		// The budget is already spent. Asking anyway would guarantee a timeout
		// and charge the caller for it.
		return excludeAfterHandshake(ReasonBudgetExhausted)
	}
	return verdict{
		relevanceBasis: manifest.RelevanceBasis,
		target: Target{
			Instance:      inst,
			Manifest:      manifest,
			Deadline:      deadline,
			TimeoutBudget: timeoutBudget,
			TimeoutLimit:  timeoutLimit,
			Limit:         perSource,
			Health:        health,
			Request:       searchReq,
			Preparation:   preparation,
		},
	}
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
func sourceDeadline(
	now, deadline time.Time,
	queryBudget, reserve, timeout time.Duration,
) (time.Time, recall.TimeoutBudget, time.Duration) {
	usable := deadline.Add(-reserve)
	if timeout > 0 {
		if own := now.Add(timeout); own.Before(usable) {
			return own, recall.TimeoutSourceLimit, timeout
		}
	}
	return usable, recall.TimeoutRequestLatency, queryBudget
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
func (p Plan) AsPlan(fusion recall.FusionRules) recall.Plan {
	out := recall.Plan{
		Profile:  p.Profile,
		Deadline: p.Deadline,
		Reserve:  p.Reserve,
		// The budget fusion applied, not the one the request carried: they
		// differ whenever a request named none, which is the case that made
		// this reportable in the first place.
		Limit:          fusion.Limit,
		RankConst:      fusion.RankConstant,
		Corrobor:       fusion.CorroborationCap,
		RelevanceFloor: fusion.RelevanceFloor,
	}
	for _, t := range p.Targets {
		out.Sources = append(out.Sources, recall.PlanSource{
			SourceUID:      t.Instance.UID,
			SourceID:       t.Instance.ID,
			RelevanceBasis: t.Manifest.RelevanceBasis,
			Eligible:       true,
			Limit:          t.Limit,
			Timeout:        time.Until(t.Deadline),
			Prior:          t.Instance.BasePrior,
		})
	}
	for _, e := range p.Excluded {
		out.Sources = append(out.Sources, recall.PlanSource{
			SourceUID:      e.SourceUID,
			SourceID:       e.SourceID,
			RelevanceBasis: p.ExcludedRelevanceBases[e.SourceUID],
			Eligible:       false,
			Reason:         e.Reason,
			Diagnostics:    e.Diagnostics,
		})
	}
	return out
}
