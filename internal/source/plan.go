package source

import (
	"context"
	"errors"
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

	// ReasonProbeTimeout is a source whose plan-time health probe did not
	// finish inside the time the plan could give it. It is not
	// [ReasonUnhealthy]: "the source is broken" and "we did not wait long
	// enough to find out" are different facts, acted on differently — one
	// wants a fix, the other a larger budget or a quieter machine. It degrades
	// coverage like every reason outside the closed non-degrading set.
	ReasonProbeTimeout       = "probe_timeout"
	ReasonAdapterUnavailable = "adapter_unavailable"
	ReasonStoreConflict      = "store_identity_conflict"

	// ReasonPanicked is a source whose adapter crashed while being considered
	// or asked. It is its own reason rather than adapter_unavailable because
	// the two are acted on differently: unavailable is a source that is not
	// there, and this is a bug in code recall ran. It degrades, which is what
	// every reason outside the closed non-degrading set does.
	ReasonPanicked = "panicked"

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
	// Granted is how long this source was given to search, measured from the
	// moment planning finished. It is what a plan reports as the source's
	// timeout: Deadline rendered against a later clock reads as negative once
	// the search is over.
	Granted time.Duration
	Limit   int

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
	queryBudget := resolveQueryBudget(req.Budget.LatencyMS, r.cfg.Defaults)
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
		// A handshake or a probe runs adapter code on the goroutine
		// [RunGuarded] starts. A panic there becomes what it is — one source
		// that cannot be asked, excluded for a reason that degrades coverage —
		// rather than a dead process.
		RunGuarded(&wg, &verdicts[i], inst.ID,
			func() verdict {
				return r.consider(
					ctx, req, inst, plan.Deadline, queryBudget, reserve, perSource, now)
			},
			func(detail string) verdict {
				return verdict{
					reason:      ReasonPanicked,
					diagnostics: map[string]any{"panic": detail},
				}
			})
	}
	wg.Wait()
	refuseDuplicateStores(instances, verdicts)
	anchorDeadlines(verdicts, now(), plan.Deadline, queryBudget, reserve)

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
	// prepared marks a target whose Request was handed to PrepareSearch. The
	// adapter may already have searched under that request's deadline, and may
	// match its result to that exact request, so [anchorDeadlines] leaves the
	// request and the budget it ran under alone.
	prepared bool
}

// anchorDeadlines starts every admitted source's search clock when planning
// finishes, not when it began (#15).
//
// Searches start only once every probe has returned, so a deadline counted
// from the start of planning charged each source for the slowest OTHER
// source's probe. A lexical source with timeout_ms = 15000 that waited 15 s at
// the barrier for a slow qmd probe reached its search with nothing left and
// reported timeout at 0 ms. Waiting on someone else is not this source being
// slow, so only the request budget — which every source shares — is charged
// for it.
//
// A source that reaches this point with the request budget already spent is
// excluded as budget_exhausted rather than asked and guaranteed to time out.
func anchorDeadlines(
	verdicts []verdict,
	release, deadline time.Time,
	queryBudget, reserve time.Duration,
) {
	for i := range verdicts {
		v := &verdicts[i]
		if v.reason != "" || v.target.Instance == nil {
			continue
		}
		at, budget, limit := sourceDeadline(
			release, deadline, queryBudget, reserve, v.target.Instance.Timeout)
		if !at.After(release) {
			// The budget is already spent. Asking anyway would guarantee a
			// timeout and charge the caller for it.
			*v = verdict{reason: ReasonBudgetExhausted, relevanceBasis: v.relevanceBasis}
			continue
		}
		v.target.Deadline = at
		if v.prepared {
			// The search may have run inside PrepareSearch, under the deadline
			// set before the probe; that is the budget that applied to it and
			// the one a timeout it returns is attributed to. The later
			// deadline only bounds collecting the result, so a finished
			// preparation is not thrown away for having waited at the barrier.
			continue
		}
		v.target.TimeoutBudget = budget
		v.target.TimeoutLimit = limit
		v.target.Granted = at.Sub(release)
		v.target.Request.Deadline = at
	}
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
	searchStart := now()
	deadline, timeoutBudget, timeoutLimit := sourceDeadline(
		searchStart, budget, queryBudget, reserve, inst.Timeout)
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
	prepared := false
	canSearchCurrent := req.AsOf == nil || manifest.AsOfSupport.Honors()
	canSearchTypes := typesOverlap(req, inst, manifest)
	probeStart := now()
	probeLimit := probeDeadline(probeStart, budget.Add(-reserve), inst.Timeout, inst.ProbeTimeout)
	probeCtx, cancelProbe := context.WithDeadline(ctx, probeLimit)
	if preparer, ok := a.(adapter.PreparedSearcher); ok &&
		deadline.After(now()) && canSearchCurrent && canSearchTypes {
		health, preparation, err = preparer.PrepareSearch(probeCtx, searchReq)
		prepared = true
	} else {
		health, err = a.Health(probeCtx)
	}
	probeExpired := errors.Is(probeCtx.Err(), context.DeadlineExceeded)
	cancelProbe()
	// A probe that ran out of time is classified by its error, not by whether
	// probeCtx has expired: an adapter may bound itself by the search deadline
	// (td's PrepareSearch does), which ends before probeCtx, and an external
	// adapter's *protocol.CallTimeout unwraps to the context's cause.
	timedOut := err != nil && (errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, protocol.ErrDeadlineExceeded))
	switch {
	case err != nil && health.Status == recall.HealthDenied:
		return excludeAfterHandshake(ReasonDenied)
	case timedOut && ctx.Err() != nil:
		// The caller's own deadline ended the probe, not the plan's limit.
		return excludeAfterHandshake(ReasonBudgetExhausted)
	case timedOut:
		v := excludeAfterHandshake(ReasonProbeTimeout)
		v.diagnostics = probeTimeoutDiagnostics(err, probeTimeoutBounds{
			probeExpired:  probeExpired,
			probeLimit:    probeLimit.Sub(probeStart),
			searchExpired: prepared && !now().Before(deadline),
			searchBudget:  timeoutBudget,
			searchLimit:   timeoutLimit,
		})
		return v
	case err != nil || !health.Usable():
		v := excludeAfterHandshake(ReasonUnhealthy)
		v.diagnostics = unhealthyDiagnostics(health, err)
		return v
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

	// Whether any budget is left is decided in [anchorDeadlines], once every
	// probe has returned and the search clock actually starts.
	return verdict{
		prepared:       prepared,
		relevanceBasis: manifest.RelevanceBasis,
		target: Target{
			Instance:      inst,
			Manifest:      manifest,
			Deadline:      deadline,
			TimeoutBudget: timeoutBudget,
			TimeoutLimit:  timeoutLimit,
			Granted:       deadline.Sub(searchStart),
			Limit:         perSource,
			Health:        health,
			Request:       searchReq,
			Preparation:   preparation,
		},
	}
}

// DefaultQueryBudget bounds a request whose caller stated none and whose
// configuration did not set defaults.budget_ms either.
const DefaultQueryBudget = 5 * time.Second

// resolveQueryBudget picks the end-to-end latency ceiling for a request.
//
// An explicit LatencyMS (CLI --budget-ms, MCP budget_ms, and so on) wins.
// Otherwise defaults.budget_ms applies when set. Otherwise the engine keeps
// DefaultQueryBudget. defaults.timeout_ms is never consulted here: it is the
// per-source inheritance default, and the builtin Timeout is 2s — short enough
// that treating it as the request budget would regress cold vector sources.
func resolveQueryBudget(latencyMS int, defaults config.Defaults) time.Duration {
	if latencyMS > 0 {
		return time.Duration(latencyMS) * time.Millisecond
	}
	if defaults.Budget > 0 {
		return defaults.Budget
	}
	return DefaultQueryBudget
}

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

// probeDeadline bounds the plan-time health probe.
//
// The probe used to run with no deadline of its own, so the adapter layer gave
// it a fixed [adapter.DefaultProbeTimeout]. Neither the source's timeout_ms nor
// the request's budget reached it, and a source whose probe is a subprocess or
// two — qmd, or an external adapter's cold first probe — crossed that fixed
// limit whenever the machine was busy and was dropped as unhealthy (#13, #14).
//
// A source's explicit probe_timeout_ms wins. Otherwise the probe gets its own
// timeout_ms, never less than the old fixed limit — so a source on the 2s
// inherited timeout is not newly starved — and never more than
// [DefaultProbeCeiling]. The ceiling exists because every search waits for the
// slowest probe (#15): a qmd source whose timeout_ms is sized for a slow
// refresh let its probe hold the whole plan for most of the request. Either way
// the limit is capped by what remains of the request once fusion's reserve is
// held back. A probe that cannot finish inside the request leaves nothing to
// search with, so waiting past it buys nothing.
func probeDeadline(now, usable time.Time, timeout, explicit time.Duration) time.Time {
	limit := explicit
	if limit <= 0 {
		limit = max(adapter.DefaultProbeTimeout, min(timeout, DefaultProbeCeiling))
	}
	if own := now.Add(limit); own.Before(usable) {
		return own
	}
	return usable
}

// DefaultProbeCeiling is the most a probe gets from a source's timeout_ms
// alone. A source whose health genuinely takes longer sets probe_timeout_ms.
const DefaultProbeCeiling = 10 * time.Second

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
			Timeout:        t.Granted,
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

// probeTimeoutBounds is what the planner knows about the deadlines a probe ran
// under when it timed out.
type probeTimeoutBounds struct {
	probeExpired  bool
	probeLimit    time.Duration
	searchExpired bool // a prepared probe also ran under the search deadline
	searchBudget  recall.TimeoutBudget
	searchLimit   time.Duration
}

// probeTimeoutDiagnostics says which bound ended a probe, so the person reading
// it knows which setting to change (#17):
//   - "adapter_call": one subprocess call ran out the adapter's own per-call
//     limit, named with the setting that controls it.
//   - "probe": the plan's probe limit (timeout_ms clamped, or probe_timeout_ms).
//   - "search_deadline": a prepared probe, which also searches (td), ran out
//     the search deadline it was given: the source's timeout_ms, or the
//     request budget when that was sooner.
//   - "adapter": any other deadline the adapter set itself, with its detail.
func probeTimeoutDiagnostics(err error, b probeTimeoutBounds) map[string]any {
	d := map[string]any{"probe_limit": b.probeLimit.String()}
	var call *adapter.CallTimeoutError
	switch {
	case errors.As(err, &call):
		d["bound"] = "adapter_call"
		d["call"] = call.Call
		d["call_limit"] = call.Limit.String()
		d["setting"] = call.Setting
	case b.probeExpired:
		d["bound"] = "probe"
	case b.searchExpired:
		d["bound"] = "search_deadline"
		d["search_limit"] = b.searchLimit.String()
		d["setting"] = "timeout_ms"
		if b.searchBudget != recall.TimeoutSourceLimit {
			d["setting"] = "budget_ms"
		}
	default:
		d["bound"] = "adapter"
		d["detail"] = err.Error()
	}
	return d
}

// unhealthyDiagnostics keeps what the source said about itself, so an
// unhealthy exclusion is never reported with no evidence.
func unhealthyDiagnostics(health recall.Health, err error) map[string]any {
	d := map[string]any{}
	for k, v := range health.Diagnostics {
		d[k] = v
	}
	if health.Status != "" {
		d["status"] = string(health.Status)
	}
	if err != nil {
		d["detail"] = err.Error()
	}
	if len(d) == 0 {
		return nil
	}
	return d
}
