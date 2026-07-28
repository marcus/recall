package eval

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// Engine is the application layer a run measures.
//
// It is an interface so a test can drive the runner without a configuration
// tree, but there is only one production implementation: evaluation runs
// through the same path as the CLI, or it would be measuring something nobody
// ships.
type Engine interface {
	Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error)
	Expand(ctx context.Context, req recall.ExpandRequest, profile string) (recall.ExpandResponse, error)
}

// SourceLocation reports where a configured source reads from, so a run can
// refuse to measure anything it cannot reproduce.
type SourceLocation struct {
	SourceID string
	Location string

	// Replayed says the source answers from a recording its adapter ships
	// with. Such a source reaches no network however its location reads: an
	// adapter over an HTTP API still has to name an endpoint it will never
	// call, and refusing it would put every network-backed adapter permanently
	// out of reach of a deterministic pack.
	Replayed bool
}

// RunOptions configure one evaluation run.
type RunOptions struct {
	// The runner has no injectable clock. A case's as_of reaches the sources on
	// the request, which is where a historical question belongs, and the only
	// other thing a runner times is how long a query took — which is wall time
	// or it is not latency. A Clock field lived here, was read by nothing but
	// that subtraction, and turned four-month-old as_of boundaries into
	// four-month latencies.

	// Locations are the configured sources, checked against the pack's network
	// policy before anything runs.
	Locations []SourceLocation

	// ExpandDetail is the detail level used when checking that returned
	// locators resolve. Excerpt is enough to prove a reference is live without
	// pulling whole records into a run artifact.
	ExpandDetail recall.DetailLevel

	// Cold says whether this run started against cold caches. Cold and warm
	// latency are never pooled: they answer different questions, and a run that
	// did not say which it was cannot be compared with either.
	Cold bool
}

// Runner executes a pack against an engine.
type Runner struct {
	engine Engine
	pack   *Pack
	opt    RunOptions
}

// NewRunner prepares a run.
func NewRunner(engine Engine, pack *Pack, opt RunOptions) *Runner {
	if opt.ExpandDetail == "" {
		opt.ExpandDetail = recall.DetailExcerpt
	}
	return &Runner{engine: engine, pack: pack, opt: opt}
}

// ErrUndeclaredNetwork means a source would have reached off this machine.
var ErrUndeclaredNetwork = errors.New("undeclared network access")

// Run executes every case and returns the per-case results.
//
// Determinism is not claimed as a property of the process; it is a property of
// what the process is pointed at. So rather than pretending to sandbox the
// network, the run refuses to start when a configured source has a remote
// location and the pack did not declare network access. A pack that measures a
// live endpoint is measuring something that changes underneath it, and its
// numbers would not be comparable between runs.
func (r *Runner) Run(ctx context.Context, cases []Case) ([]CaseResult, error) {
	if err := r.checkNetworkPolicy(); err != nil {
		return nil, err
	}

	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		result, err := r.runCase(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.CaseID, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (r *Runner) checkNetworkPolicy() error {
	if r.pack.NetworkAccess {
		return nil
	}
	for _, loc := range r.opt.Locations {
		if loc.Replayed {
			continue // answers from a recording, so the endpoint is never called
		}
		u, err := url.Parse(loc.Location)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue // a path, not an endpoint
		}
		return fmt.Errorf("%w: source %q reads from %s, and pack %q declares network_access false",
			ErrUndeclaredNetwork, loc.SourceID, u.Scheme+"://"+u.Host, r.pack.PackID)
	}
	return nil
}

// runCase puts one case to the engine and reduces the response to what metrics
// consume.
func (r *Runner) runCase(ctx context.Context, c Case) (CaseResult, error) {
	// A case with an as_of is asking a historical question, and the boundary
	// travels on the request so a source cannot treat it as future-dated and
	// quietly answer from current state.
	req := recall.QueryRequest{
		Query:   c.Query,
		Profile: c.Profile,
		AsOf:    c.AsOf,
		Mode:    recall.ModeExplicit,
		Limit:   defaultCaseLimit,
		Budget:  recall.Budget{LatencyMS: c.TimeoutMS},
	}
	if c.Mode == recall.ModePreReply {
		req.Mode = recall.ModePreReply
		req.SuppressLineages = c.SuppressLineages
	}
	if c.Scope != nil {
		req.Scope = &recall.Scope{
			SourceIDs:   c.Scope.SourceIDs,
			RecordTypes: c.Scope.RecordTypes,
			Entities:    c.Scope.Entities,
			Project:     c.Scope.Project,
		}
	}

	// Latency is wall time, never the case's clock. A pinned clock is pinned
	// for the sources, so they answer the historical question; it says nothing
	// about how long the query took. Reading it as a duration subtracted the
	// as_of instant from the real one and reported the age of the boundary —
	// the March as_of cases each measured a "latency" of four months. Two of
	// them sat just past the nearest-rank p95 position, so the budget gate was
	// passing on the arithmetic of where the outliers landed rather than on
	// anything the engine did.
	started := time.Now()
	resp, queryErr := r.engine.Query(ctx, req)
	elapsed := time.Since(started)

	result := CaseResult{
		CaseID:   c.CaseID,
		Behavior: behaviorOf(resp.Outcome),
		Coverage: resp.Coverage,
		Latency:  elapsed,
		Cold:     r.opt.Cold,
	}
	if queryErr != nil {
		// A failed query is a result, not an error in the run: a pack exists in
		// part to check what happens when things break.
		result.Behavior = BehaviorAbstain
		result.Error = queryErr.Error()
	}

	for _, res := range resp.Results {
		result.Ranked = append(result.Ranked, positions(res)...)
		result.Provenance = append(result.Provenance, provenanceOf(res)...)
	}
	result.Results = resultRefs(c, resp)
	if len(resp.SourceOutcomes) > 0 {
		result.SourceOutcomes = make(map[recall.SourceUID]recall.SearchOutcome, len(resp.SourceOutcomes))
		for _, so := range resp.SourceOutcomes {
			result.SourceOutcomes[so.SourceUID] = so.Outcome
		}
	}
	result.SourceFamilies = familiesOf(resp)
	result.ReturnedSources = returnedSourcesOf(resp)
	result.Suppressions = append([]recall.Suppression(nil), resp.Suppressed...)
	result.SensitivityViolations = ceilingViolations(c, resp)
	result.Expansions = r.checkExpansions(ctx, c, resp)
	return result, nil
}

const defaultCaseLimit = 20

// positions projects a cluster onto ranked positions.
//
// One position per lineage root the result displays, not one per cluster: a
// cluster backed by two distinct records contributed two pieces of evidence,
// and collapsing them to one position would understate recall for exactly the
// fused results the system exists to produce. Two roots that are one record —
// a view the response reported as `duplicate_view` — are the case this cannot
// tell apart from here, and [Score] collapses them onto one position using
// what the response said about them.
func positions(res recall.Result) []recall.LineageRoot {
	if len(res.Members) == 0 {
		return []recall.LineageRoot{res.Explanation.LineageRoot}
	}
	out := make([]recall.LineageRoot, 0, len(res.Members))
	for _, m := range res.Members {
		out = append(out, m.LineageRoot)
	}
	return out
}

// resultRefs records what a case may assert about the results themselves: how
// many came back, which record each one is, and what it displayed.
//
// Record identity is the primary candidate's, because the primary is what a
// caller reads. An excerpt is captured only for a root the case asked about,
// so the artifact carries the text an assertion needs and no more.
func resultRefs(c Case, resp recall.QueryResponse) []ResultRef {
	var asked map[recall.LineageRoot][]string
	if c.Assertions != nil {
		asked = c.Assertions.ExcerptContains
	}
	refs := make([]ResultRef, 0, len(resp.Results))
	for _, res := range resp.Results {
		ref := ResultRef{
			Root:        res.Explanation.LineageRoot,
			RecordID:    res.Primary.SourceRecordID,
			Fingerprint: res.Primary.ContentFingerprint,
		}
		if _, want := asked[ref.Root]; want {
			ref.Excerpt = res.Primary.Excerpt
		}
		refs = append(refs, ref)
	}
	return refs
}

// provenanceOf checks that each candidate named where it really came from.
//
// The comparison is per member, against that member's own lineage root. An
// earlier version compared every candidate against the cluster's primary root,
// which marked every correctly fused multi-record cluster as a provenance
// failure — the metric punished exactly the behavior the system exists to
// produce, and read 0.65 on a run where nothing was wrong.
//
// The source is checked against the source encoded in the candidate's own
// locator rather than against its lineage root: a projection legitimately sits
// under an upstream record's root, so a mismatch there is lineage working, not
// a candidate lying about itself.
func provenanceOf(res recall.Result) []Provenance {
	var out []Provenance
	for _, m := range res.Members {
		for _, c := range m.Candidates {
			out = append(out, Provenance{
				ClaimedSource: c.SourceUID,
				ActualSource:  sourceOfLocator(c, c.SourceUID),
				ClaimedRoot:   m.LineageRoot,
				ActualRoot:    resolvedRoot(m.LineageRoot),
			})
		}
	}
	return out
}

// sourceOfLocator recovers the source a candidate's own locator names.
func sourceOfLocator(c recall.Candidate, fallback recall.SourceUID) recall.SourceUID {
	if c.Locator.SourceUID != "" {
		return c.Locator.SourceUID
	}
	return fallback
}

// resolvedRoot returns the root when it is well formed, and nothing when it is
// not: a root that does not parse cannot be resolved back to a record, which is
// the failure this metric is for.
func resolvedRoot(root recall.LineageRoot) recall.LineageRoot {
	if _, err := root.Locator(); err != nil {
		return ""
	}
	return root
}

// ceilingViolations counts returned candidates above the ceiling the case
// asserted.
//
// It counts what was RETURNED, never what was suppressed: a suppression is the
// control working, and scoring it as a violation would punish it for existing.
// A case that asserts no ceiling cannot be violated, so it reports zero rather
// than guessing one.
func ceilingViolations(c Case, resp recall.QueryResponse) int {
	if c.Assertions == nil || c.Assertions.SensitivityCeiling == nil {
		return 0
	}
	ceiling := *c.Assertions.SensitivityCeiling
	violations := 0
	for _, res := range resp.Results {
		if !res.Primary.Sensitivity.AtMost(ceiling) {
			violations++
			continue
		}
		for _, m := range res.Members {
			for _, cand := range m.Candidates {
				if !cand.Sensitivity.AtMost(ceiling) {
					violations++
				}
			}
		}
	}
	return violations
}

// familiesOf names the sources that contributed, so a regression can be read
// per source family rather than only in aggregate.
func familiesOf(resp recall.QueryResponse) []string {
	seen := map[string]bool{}
	var out []string
	for _, res := range resp.Results {
		if id := res.Primary.SourceID; id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// returnedSourcesOf names immutable source identities that contributed
// candidates. A primary is included even when a synthetic or older adapter
// omitted member candidates; members are included so fused evidence keeps all
// of its contributing sources.
func returnedSourcesOf(resp recall.QueryResponse) []recall.SourceUID {
	seen := map[recall.SourceUID]bool{}
	for _, res := range resp.Results {
		if res.Primary.SourceUID != "" {
			seen[res.Primary.SourceUID] = true
		}
		for _, member := range res.Members {
			for _, candidate := range member.Candidates {
				if candidate.SourceUID != "" {
					seen[candidate.SourceUID] = true
				}
			}
		}
	}
	out := make([]recall.SourceUID, 0, len(seen))
	for uid := range seen {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// checkExpansions proves the references a run returned are live.
//
// A ranking that returns unusable locators has not retrieved anything, however
// well ordered it is, so this is a first-class measurement rather than a
// diagnostic.
func (r *Runner) checkExpansions(ctx context.Context, c Case, resp recall.QueryResponse) []Expansion {
	var out []Expansion
	var budget int64
	if c.Assertions != nil {
		budget = c.Assertions.MaxExpansionBytes
	}
	for _, res := range resp.Results {
		loc := res.Primary.Locator
		exp := Expansion{
			Locator:   loc,
			SourceUID: res.Primary.SourceUID,
		}
		got, err := r.engine.Expand(ctx, recall.ExpandRequest{
			Locator: loc,
			Detail:  r.opt.ExpandDetail,
			Budget:  budget,
		}, c.Profile)
		if err == nil {
			exp.Root = res.Explanation.LineageRoot
			exp.Revision = got.SourceRevision
			exp.Bytes = int64(len(got.Content))
		}
		out = append(out, exp)
	}
	return out
}

// behaviorOf maps a response outcome onto the behavior a pack judges.
//
// The two vocabularies are deliberately the same size. An earlier draft had a
// "clarify" behavior with no outcome behind it, so a case expecting it could
// never pass: Recall retrieves, and deciding to ask a follow-up question is
// something a host does with what Recall returned.
func behaviorOf(o recall.Outcome) Behavior {
	switch o {
	case recall.OutcomeAnswered:
		return BehaviorAnswer
	case recall.OutcomeAbstained:
		return BehaviorAbstain
	case recall.OutcomeFailed:
		return BehaviorFail
	default:
		return BehaviorFail
	}
}

// SortResults orders results by case id so a run artifact is diffable.
func SortResults(in []CaseResult) {
	sort.Slice(in, func(i, j int) bool { return in[i].CaseID < in[j].CaseID })
}
