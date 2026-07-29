package eval

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/recall"
)

// Cut points. They are constants rather than parameters because a report whose
// k varied between runs would not be comparable, which is the only thing a
// report is for. Gates that need another cut point call the metric directly.
const (
	ndcgK       = 10
	recallK     = 5
	recallDeepK = 20
	mrrK        = 10
	successK    = 5
	forbiddenK  = 5
)

// Expansion is what one returned reference actually expanded to. The runner
// performs the expansion against the fixture; this package only compares.
type Expansion struct {
	// Locator is the reference the run returned, in display form.
	Locator recall.Locator `json:"locator"`

	// SourceUID is the identity behind that reference. Display form drops it,
	// and a run artifact that recorded only the display form would stop
	// resolving the moment a source was renamed — the exact failure source_uid
	// exists to prevent.
	SourceUID recall.SourceUID `json:"source_uid,omitempty"`

	// Root is the lineage root the expansion resolved to, empty when it did not
	// resolve at all.
	Root recall.LineageRoot `json:"lineage_root,omitempty"`

	// Revision is the source revision the expansion landed on. Resolving to the
	// right record at the wrong revision is a locator failure, not a near miss.
	Revision string `json:"source_revision,omitempty"`

	// Bytes is the size of the expanded content, not its JSON envelope. Case
	// byte ceilings use the same unit as ExpandRequest.Budget.
	Bytes int64 `json:"bytes,omitempty"`
}

// Provenance is one returned candidate's claimed origin beside the fixture's
// truth. Both halves are recorded rather than a bare verdict so a report can
// say what was claimed, not only that something was wrong.
type Provenance struct {
	ClaimedSource recall.SourceUID   `json:"claimed_source_uid"`
	ActualSource  recall.SourceUID   `json:"actual_source_uid"`
	ClaimedRoot   recall.LineageRoot `json:"claimed_lineage_root"`
	ActualRoot    recall.LineageRoot `json:"actual_lineage_root"`
}

// Correct reports whether the candidate named where it really came from.
func (p Provenance) Correct() bool {
	return p.ClaimedSource == p.ActualSource && p.ClaimedRoot == p.ActualRoot
}

// CaseResult is one case's observed outcome, reduced to exactly what metrics
// consume. The runner builds it; metrics never see clusters, candidates,
// excerpts, or rendered text.
type CaseResult struct {
	CaseID string `json:"case_id"`

	// Ranked is the final result order, one lineage root per position. The
	// runner projects clusters onto positions; a cluster contributing several
	// independent records contributes several positions, in display order.
	Ranked []recall.LineageRoot `json:"ranked"`

	// Results is one entry per returned result, in display order. It counts
	// something different from Ranked: a cluster fusing two records is one
	// result and two ranked positions, and a case asserting what a caller was
	// handed means results.
	Results []ResultRef `json:"results,omitempty"`

	// SourceFamilies names the source families that contributed to this
	// result. A case belongs to every family it touched, so a family's metrics
	// cover every case it had a hand in.
	SourceFamilies []string `json:"source_families,omitempty"`

	// Behavior is what Recall actually did: answered, abstained, or failed.
	Behavior Behavior `json:"behavior"`

	// Error is the query error, when the request failed outright. A failed
	// query is a result and not a broken run: a pack exists in part to check
	// what happens when things break.
	Error string `json:"error,omitempty"`

	// SensitivityViolations counts returned candidates above the ceiling the
	// case asserted. It counts what was returned and never what was
	// suppressed, or the control would be punished for working.
	SensitivityViolations int `json:"sensitivity_violations,omitempty"`

	// Coverage is what the response reported. The truth it is checked against
	// comes from the case's assertions, never from the response.
	Coverage recall.Coverage `json:"coverage"`

	// SourceOutcomes is what each source reported for this case.
	SourceOutcomes map[recall.SourceUID]recall.SearchOutcome `json:"source_outcomes,omitempty"`

	// ReturnedSources is the set of immutable source identities that actually
	// contributed candidates. Source outcomes alone are insufficient: a source
	// may report success while contributing no evidence.
	ReturnedSources []recall.SourceUID `json:"returned_source_uids,omitempty"`

	// Suppressions are the engine's positive evidence that a retrieved
	// candidate was withheld, including the policy reason and candidate count.
	// Absence from Ranked is not proof of suppression: the candidate may never
	// have been retrieved.
	Suppressions []recall.Suppression `json:"suppressions,omitempty"`

	Expansions []Expansion  `json:"expansions,omitempty"`
	Provenance []Provenance `json:"provenance,omitempty"`

	Latency time.Duration `json:"latency_ns"`

	// Cold says whether this case ran against cold caches. Cold and warm
	// latency are never pooled: their distributions are different questions.
	Cold bool `json:"cold"`
}

// ResultRef is what a case can assert about one returned result: which record
// it is, and what its excerpt said.
//
// Excerpt is filled only for the lineage roots a case named in
// excerpt_contains. A run artifact holding every excerpt would hold the corpus,
// and the assertion is the only thing that reads one.
type ResultRef struct {
	Root        recall.LineageRoot `json:"lineage_root"`
	RecordID    string             `json:"source_record_id,omitempty"`
	Fingerprint string             `json:"content_fingerprint,omitempty"`
	Excerpt     string             `json:"excerpt,omitempty"`
}

// CaseScore is every metric that is defined for one case, plus the groups the
// case belongs to.
type CaseScore struct {
	CaseID         string   `json:"case_id"`
	Tags           []string `json:"tags,omitempty"`
	SourceFamilies []string `json:"source_families,omitempty"`

	NDCG10     Value `json:"ndcg_at_10"`
	Recall5    Value `json:"recall_at_5"`
	Recall20   Value `json:"recall_at_20"`
	MRR10      Value `json:"mrr_at_10"`
	Success5   Value `json:"success_at_5"`
	Forbidden5 Value `json:"forbidden_at_5"`

	AbstentionAccuracy    Value `json:"abstention_accuracy"`
	CoverageAccuracy      Value `json:"coverage_accuracy"`
	LocatorSuccess        Value `json:"locator_success"`
	ProvenanceAccuracy    Value `json:"provenance_accuracy"`
	SourceOutcomeAccuracy Value `json:"source_outcome_accuracy"`

	// AssertionViolations names every declared source, lineage, latency, or
	// expansion assertion the observed result violated. They feed a hard gate:
	// a case-level contract is never diluted into an aggregate.
	AssertionViolations []string `json:"assertion_violations,omitempty"`

	// ExpectedFail carries the case's expected-failure marking. It is on the
	// score because gates read scores: the assertion gate has to know which
	// violations were declared in advance and which were not, and a report has
	// to name what each one waits on.
	ExpectedFail *ExpectedFail `json:"expected_fail,omitempty"`

	// SensitivityViolations is carried onto the score because it is a gate
	// input, and a gate must not have to re-read raw results to decide.
	SensitivityViolations int `json:"sensitivity_violations,omitempty"`

	Latency time.Duration `json:"latency_ns"`
	Cold    bool          `json:"cold"`
}

// Mean is an average and how many observations produced it.
//
// N is part of the value, not decoration. A metric undefined for every
// observation has N of 0 and a meaningless Value; a caller that reads it as
// zero has invented a regression out of an absence of evidence.
type Mean struct {
	Value float64 `json:"value"`
	N     int     `json:"n"`
}

// Defined reports whether anything was averaged.
func (m Mean) Defined() bool { return m.N > 0 }

// Rates is the metric vocabulary: every measure that is a fraction and so can
// be averaged again across groups. It is a separate type from [Metrics]
// because latency cannot — a percentile of percentiles is not a percentile.
type Rates struct {
	NDCG10     Mean `json:"ndcg_at_10"`
	Recall5    Mean `json:"recall_at_5"`
	Recall20   Mean `json:"recall_at_20"`
	MRR10      Mean `json:"mrr_at_10"`
	Success5   Mean `json:"success_at_5"`
	Forbidden5 Mean `json:"forbidden_at_5"`

	AbstentionAccuracy    Mean `json:"abstention_accuracy"`
	CoverageAccuracy      Mean `json:"coverage_accuracy"`
	LocatorSuccess        Mean `json:"locator_success"`
	ProvenanceAccuracy    Mean `json:"provenance_accuracy"`
	SourceOutcomeAccuracy Mean `json:"source_outcome_accuracy"`
}

// Percentiles is a latency distribution's shape, in nanoseconds.
type Percentiles struct {
	N   int           `json:"n"`
	P50 time.Duration `json:"p50_ns"`
	P95 time.Duration `json:"p95_ns"`
}

// LatencyStats keeps cold and warm apart. Pooling them reports a distribution
// that describes neither.
type LatencyStats struct {
	Cold Percentiles `json:"cold"`
	Warm Percentiles `json:"warm"`
}

// Metrics is one population's numbers: every case in a run, or every case in
// one group.
type Metrics struct {
	Rates
	Cases   int          `json:"cases"`
	Latency LatencyStats `json:"latency"`
}

// Macro is the unweighted mean of each rate across groups: every group counts
// once, however many cases it holds. It is the number that keeps one large
// document source from hiding a regression in exact lookups, temporal
// questions, or no-answer cases.
//
// Mean.N counts contributing groups here, not cases. Latency is absent by
// construction.
type Macro struct {
	Rates
	Groups int `json:"groups"`
}

// GroupReport is one grouping dimension: the per-group numbers and the macro
// average over them.
type GroupReport struct {
	Groups map[string]Metrics `json:"groups"`
	Macro  Macro              `json:"macro"`
}

// Report is a run's metrics: pooled over every case, and grouped along the two
// dimensions that hide different regressions — what a case asks for, and where
// its evidence came from.
type Report struct {
	Cases          int         `json:"cases"`
	Overall        Metrics     `json:"overall"`
	ByTag          GroupReport `json:"by_tag"`
	BySourceFamily GroupReport `json:"by_source_family"`
}

// rateField pairs a per-case metric with the average it feeds. One table
// rather than two keeps aggregation and macro averaging from drifting apart,
// and a metric added to [Rates] but not here is caught by
// TestEveryRateIsAggregated.
type rateField struct {
	name  string
	score func(*CaseScore) Value
	mean  func(*Rates) *Mean
}

func rateFields() []rateField {
	return []rateField{
		{"ndcg_at_10", func(s *CaseScore) Value { return s.NDCG10 }, func(r *Rates) *Mean { return &r.NDCG10 }},
		{"recall_at_5", func(s *CaseScore) Value { return s.Recall5 }, func(r *Rates) *Mean { return &r.Recall5 }},
		{"recall_at_20", func(s *CaseScore) Value { return s.Recall20 }, func(r *Rates) *Mean { return &r.Recall20 }},
		{"mrr_at_10", func(s *CaseScore) Value { return s.MRR10 }, func(r *Rates) *Mean { return &r.MRR10 }},
		{"success_at_5", func(s *CaseScore) Value { return s.Success5 }, func(r *Rates) *Mean { return &r.Success5 }},
		{"forbidden_at_5", func(s *CaseScore) Value { return s.Forbidden5 }, func(r *Rates) *Mean { return &r.Forbidden5 }},
		{"abstention_accuracy", func(s *CaseScore) Value { return s.AbstentionAccuracy }, func(r *Rates) *Mean { return &r.AbstentionAccuracy }},
		{"coverage_accuracy", func(s *CaseScore) Value { return s.CoverageAccuracy }, func(r *Rates) *Mean { return &r.CoverageAccuracy }},
		{"locator_success", func(s *CaseScore) Value { return s.LocatorSuccess }, func(r *Rates) *Mean { return &r.LocatorSuccess }},
		{"provenance_accuracy", func(s *CaseScore) Value { return s.ProvenanceAccuracy }, func(r *Rates) *Mean { return &r.ProvenanceAccuracy }},
		{"source_outcome_accuracy", func(s *CaseScore) Value { return s.SourceOutcomeAccuracy }, func(r *Rates) *Mean { return &r.SourceOutcomeAccuracy }},
	}
}

// Score computes every metric defined for one case.
//
// judgments may be the whole pack's; those naming another case are ignored, so
// a caller cannot get a wrong answer by forgetting to filter.
func Score(c Case, judgments []Judgment, r CaseResult) CaseScore {
	grades := make(Grades)
	required := make(RootSet)
	forbidden := make(RootSet)
	useful := make(RootSet)
	authoritative := make(RootSet)

	for _, j := range judgments {
		if j.CaseID != c.CaseID {
			continue
		}
		grades[j.LineageRoot] = j.Relevance
		if j.Required {
			required[j.LineageRoot] = true
		}
		if j.Forbidden {
			forbidden[j.LineageRoot] = true
		}
		if j.Relevance >= UsefulSupport {
			useful[j.LineageRoot] = true
		}
		if j.Relevance == Authoritative {
			authoritative[j.LineageRoot] = true
		}
	}

	s := CaseScore{
		CaseID:                c.CaseID,
		Tags:                  c.Tags,
		SourceFamilies:        r.SourceFamilies,
		SensitivityViolations: r.SensitivityViolations,
		Latency:               r.Latency,
		Cold:                  r.Cold,
	}

	// Ranking is measured over records, not roots: what a caller was handed is
	// a list of positions, and two views of one record are one of them.
	canon := records(r.Suppressions)
	ranked := canonicalRoots(r.Ranked, canon)
	grades = canonicalGrades(grades, canon)
	required = canonicalSet(required, canon)
	forbidden = canonicalSet(forbidden, canon)
	useful = canonicalSet(useful, canon)
	authoritative = canonicalSet(authoritative, canon)

	s.NDCG10 = value(NDCGAt(ranked, grades, ndcgK))
	s.Recall5 = value(RecallAt(ranked, required, recallK))
	s.Recall20 = value(RecallAt(ranked, required, recallDeepK))
	s.MRR10 = value(MRRAt(ranked, authoritative, mrrK))

	// Success is measured against evidence the pack says is worth finding. A
	// case with nothing useful to find cannot succeed or fail at finding it.
	if len(useful) > 0 {
		s.Success5 = boolValue(HitAt(ranked, useful, successK))
	}
	// Likewise, a case that names no forbidden evidence was never at risk of
	// surfacing any, and counting it as a clean pass would dilute the rate.
	if len(forbidden) > 0 {
		s.Forbidden5 = boolValue(HitAt(ranked, forbidden, forbiddenK))
	}

	if c.ExpectedBehavior.Valid() {
		s.AbstentionAccuracy = boolValue(r.Behavior == c.ExpectedBehavior)
	}
	s.CoverageAccuracy = coverageAccuracy(c, r)
	s.LocatorSuccess = locatorSuccess(c, r)
	s.ProvenanceAccuracy = provenanceAccuracy(r)
	s.SourceOutcomeAccuracy = sourceOutcomeAccuracy(c, r)
	s.AssertionViolations = caseAssertionViolations(c, r)
	s.ExpectedFail = c.ExpectedFail

	return s
}

func value(v float64, ok bool) Value { return Value{V: v, OK: ok} }

// records maps every lineage root the response reported as a folded view onto
// the root of the result it was folded into.
//
// A `duplicate_view` suppression is the response saying two roots are one
// record that took one result slot. The projection onto rank positions keeps
// both — both are true accounts of where the record was read, and either one
// expands — so scoring has to be told, or one caller-visible result is counted
// twice: it takes two positions in the ranking, and every position behind it
// is measured at a rank no caller ever saw.
//
// It is read from the run rather than declared by the pack because it is a
// fact about the answer and not about the corpus. Both sides are mapped, so a
// judgment naming either root credits or forbids the one slot displayed, and a
// forbidden root folded into a shown result still reports as shown.
func records(suppressions []recall.Suppression) map[recall.LineageRoot]recall.LineageRoot {
	var canon map[recall.LineageRoot]recall.LineageRoot
	for _, s := range suppressions {
		if s.Reason != recall.SuppressDuplicateView || s.LineageRoot == "" || s.FusedInto == "" {
			continue
		}
		if canon == nil {
			canon = make(map[recall.LineageRoot]recall.LineageRoot, len(suppressions))
		}
		canon[s.LineageRoot] = s.FusedInto
	}
	return canon
}

// canonicalRoots rewrites a ranking in terms of records, keeping the first
// position each one reached. A response that fused nothing is returned as it
// stands.
func canonicalRoots(ranked []recall.LineageRoot, canon map[recall.LineageRoot]recall.LineageRoot) []recall.LineageRoot {
	if len(canon) == 0 {
		return ranked
	}
	out := make([]recall.LineageRoot, 0, len(ranked))
	seen := make(RootSet, len(ranked))
	for _, root := range ranked {
		if got, ok := canon[root]; ok {
			root = got
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out
}

func canonicalSet(set RootSet, canon map[recall.LineageRoot]recall.LineageRoot) RootSet {
	if len(canon) == 0 || len(set) == 0 {
		return set
	}
	out := make(RootSet, len(set))
	for root := range set {
		if got, ok := canon[root]; ok {
			root = got
		}
		out[root] = true
	}
	return out
}

// canonicalGrades takes the highest grade any view of a record was given: the
// record is as relevant as the best account of it says it is.
func canonicalGrades(grades Grades, canon map[recall.LineageRoot]recall.LineageRoot) Grades {
	if len(canon) == 0 || len(grades) == 0 {
		return grades
	}
	out := make(Grades, len(grades))
	for root, g := range grades {
		if got, ok := canon[root]; ok {
			root = got
		}
		if prev, seen := out[root]; !seen || g > prev {
			out[root] = g
		}
	}
	return out
}

// coverageAccuracy checks degraded coverage was reported when and only when it
// was true. Both directions are errors: claiming degradation that did not
// happen teaches a caller to ignore the signal just as surely as hiding
// degradation that did.
func coverageAccuracy(c Case, r CaseResult) Value {
	if c.Assertions == nil || c.Assertions.ExpectedCoverage == "" {
		return Value{}
	}
	return boolValue(r.Coverage == c.Assertions.ExpectedCoverage)
}

// locatorSuccess is the fraction of returned references that expanded to a
// real record, at the revision the pack demands when it demands one.
func locatorSuccess(c Case, r CaseResult) Value {
	if len(r.Expansions) == 0 {
		return Value{}
	}
	var wantRevision map[recall.LineageRoot]string
	if c.Assertions != nil {
		wantRevision = c.Assertions.ExpectedRevisions
	}

	good := 0
	for _, e := range r.Expansions {
		if e.Root == "" {
			continue
		}
		if want, declared := wantRevision[e.Root]; declared && want != e.Revision {
			continue
		}
		good++
	}
	return defined(float64(good) / float64(len(r.Expansions)))
}

// provenanceAccuracy is the fraction of returned candidates that named the
// source and lineage root they really came from.
func provenanceAccuracy(r CaseResult) Value {
	if len(r.Provenance) == 0 {
		return Value{}
	}
	good := 0
	for _, p := range r.Provenance {
		if p.Correct() {
			good++
		}
	}
	return defined(float64(good) / float64(len(r.Provenance)))
}

// sourceOutcomeAccuracy is the fraction of sources whose reported outcome
// matched what the fixture made them do. A source that never reported at all
// counts as wrong: silence about a denied or timed-out source is exactly the
// dishonesty this measures.
func sourceOutcomeAccuracy(c Case, r CaseResult) Value {
	if c.Assertions == nil || len(c.Assertions.ExpectedSourceOutcomes) == 0 {
		return Value{}
	}
	expected := c.Assertions.ExpectedSourceOutcomes
	good := 0
	for uid, want := range expected {
		if r.SourceOutcomes[uid] == want {
			good++
		}
	}
	return defined(float64(good) / float64(len(expected)))
}

// caseAssertionViolations evaluates the case fields whose contract is categorical
// or case-local rather than an aggregate ranking metric. Each declared field
// reaches this function; a failed assertion is recorded by name and later
// invalidates the run.
func caseAssertionViolations(c Case, r CaseResult) []string {
	if c.Assertions == nil {
		return nil
	}
	a := c.Assertions
	var failures []string

	sources := make(map[recall.SourceUID]bool, len(r.ReturnedSources))
	for _, uid := range r.ReturnedSources {
		sources[uid] = true
	}
	for _, uid := range a.RequiredSources {
		if !sources[uid] {
			failures = append(failures, fmt.Sprintf("required_sources: missing %s", uid))
		}
	}
	for _, uid := range a.ForbiddenSources {
		if sources[uid] {
			failures = append(failures, fmt.Sprintf("forbidden_sources: returned %s", uid))
		}
	}

	if a.MaxLatencyMS > 0 && r.Latency > time.Duration(a.MaxLatencyMS)*time.Millisecond {
		failures = append(failures, fmt.Sprintf(
			"max_latency_ms: %s exceeded %dms", r.Latency, a.MaxLatencyMS))
	}
	if a.MaxExpansionBytes > 0 {
		if len(r.Expansions) == 0 && c.ExpectedBehavior != BehaviorAbstain {
			failures = append(failures,
				"max_expansion_bytes: no returned expansion to measure")
		} else {
			for _, expansion := range r.Expansions {
				if expansion.Bytes > a.MaxExpansionBytes {
					failures = append(failures, fmt.Sprintf(
						"max_expansion_bytes: %s returned %d bytes over %d",
						expansion.Locator.Local, expansion.Bytes, a.MaxExpansionBytes))
				}
			}
		}
	}

	ranked := make(RootSet, len(r.Ranked))
	for _, root := range r.Ranked {
		ranked[root] = true
	}
	expanded := make(map[recall.LineageRoot]Expansion, len(r.Expansions))
	for _, expansion := range r.Expansions {
		if expansion.Root != "" {
			expanded[expansion.Root] = expansion
		}
	}
	for root, want := range a.ExpectedRevisions {
		expansion, ok := expanded[root]
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"expected_revisions: no expansion resolved %s", root))
			continue
		}
		if expansion.Revision != want {
			failures = append(failures, fmt.Sprintf(
				"expected_revisions: %s resolved revision %q, want %q",
				root, expansion.Revision, want))
		}
	}

	suppressed := make(RootSet, len(r.Suppressions))
	for _, suppression := range r.Suppressions {
		if suppression.Reason == recall.SuppressLineageSeen &&
			suppression.Count > 0 &&
			suppression.LineageRoot != "" {
			suppressed[suppression.LineageRoot] = true
		}
	}
	for _, root := range a.SuppressedLineages {
		if ranked[root] {
			failures = append(failures, fmt.Sprintf(
				"suppressed_lineages: returned %s", root))
		} else if !suppressed[root] {
			failures = append(failures, fmt.Sprintf(
				"suppressed_lineages: no suppression telemetry for %s with reason %s",
				root, recall.SuppressLineageSeen))
		}
	}
	// Keyed by root AND reason: "withheld" without saying by what is satisfied
	// by any rule that happens to remove the record, which is the ambiguity this
	// assertion exists to close.
	withheldBy := make(map[recall.LineageRoot]string, len(r.Suppressions))
	for _, suppression := range r.Suppressions {
		if suppression.Count > 0 && suppression.LineageRoot != "" {
			withheldBy[suppression.LineageRoot] = suppression.Reason
		}
	}
	displayed := make(RootSet, len(r.Results))
	for _, result := range r.Results {
		displayed[result.Root] = true
	}
	for _, root := range sortedRoots(a.WithheldLineages) {
		want := a.WithheldLineages[root]
		switch got := withheldBy[root]; {
		case displayed[root]:
			failures = append(failures, fmt.Sprintf(
				"withheld_lineages: returned %s", root))
		case got == "":
			failures = append(failures, fmt.Sprintf(
				"withheld_lineages: no suppression telemetry for %s", root))
		case got != want:
			failures = append(failures, fmt.Sprintf(
				"withheld_lineages: %s withheld as %s, want %s", root, got, want))
		}
	}
	for _, root := range a.VisibleLineages {
		if !ranked[root] {
			failures = append(failures, fmt.Sprintf(
				"visible_lineages: missing %s", root))
		}
	}
	failures = append(failures, resultAssertionViolations(a, r.Results)...)

	sort.Strings(failures)
	return failures
}

// resultAssertionViolations evaluates the assertions about the result list a
// caller was handed: its head, its length, its duplication, and its text.
func resultAssertionViolations(a *Assertions, results []ResultRef) []string {
	var failures []string

	if a.ExpectedTopLineage != "" {
		switch {
		case len(results) == 0:
			failures = append(failures, fmt.Sprintf(
				"expected_top_lineage: %s did not rank; nothing was returned", a.ExpectedTopLineage))
		case results[0].Root != a.ExpectedTopLineage:
			failures = append(failures, fmt.Sprintf(
				"expected_top_lineage: %s ranked first, want %s",
				results[0].Root, a.ExpectedTopLineage))
		}
	}
	if a.MinResults != nil && len(results) < *a.MinResults {
		failures = append(failures, fmt.Sprintf(
			"min_results: returned %d, want at least %d", len(results), *a.MinResults))
	}
	if a.MaxResults != nil && len(results) > *a.MaxResults {
		failures = append(failures, fmt.Sprintf(
			"max_results: returned %d, want at most %d", len(results), *a.MaxResults))
	}
	if a.MaxResultsPerRecord != nil {
		// A record is identified across sources only when it presents both
		// halves of the evidence. Missing either one, a result stands alone
		// rather than being merged on a guess.
		occupied := map[string]int{}
		for _, ref := range results {
			if ref.RecordID == "" || ref.Fingerprint == "" {
				continue
			}
			occupied[ref.RecordID+"\x00"+ref.Fingerprint]++
		}
		for key, n := range occupied {
			if n > *a.MaxResultsPerRecord {
				id, _, _ := strings.Cut(key, "\x00")
				failures = append(failures, fmt.Sprintf(
					"max_results_per_record: record %s occupied %d results, want at most %d",
					id, n, *a.MaxResultsPerRecord))
			}
		}
	}
	for root, wants := range a.ExcerptContains {
		ref, ok := excerptOf(results, root)
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"excerpt_contains: %s returned no result to read", root))
			continue
		}
		for _, want := range wants {
			if !strings.Contains(strings.ToLower(ref), strings.ToLower(want)) {
				failures = append(failures, fmt.Sprintf(
					"excerpt_contains: %s displayed an excerpt without %q", root, want))
			}
		}
	}
	return failures
}

func excerptOf(results []ResultRef, root recall.LineageRoot) (string, bool) {
	for _, ref := range results {
		if ref.Root == root {
			return ref.Excerpt, true
		}
	}
	return "", false
}

// Scores computes one CaseScore per result.
//
// A result naming a case the pack does not contain is an error rather than a
// skip: it means the runner and the pack disagree about what was run, and
// every number after that is untrustworthy.
func Scores(cases []Case, judgments []Judgment, results []CaseResult) ([]CaseScore, error) {
	byID := make(map[string]Case, len(cases))
	for _, c := range cases {
		byID[c.CaseID] = c
	}
	byCase := make(map[string][]Judgment, len(cases))
	for _, j := range judgments {
		byCase[j.CaseID] = append(byCase[j.CaseID], j)
	}

	out := make([]CaseScore, 0, len(results))
	for _, r := range results {
		c, ok := byID[r.CaseID]
		if !ok {
			return nil, fmt.Errorf("result for unknown case %q", r.CaseID)
		}
		out = append(out, Score(c, byCase[r.CaseID], r))
	}
	return out, nil
}

// ReportOf pools scores overall and groups them by case tag and by source
// family, with an unweighted macro average over each dimension's groups.
func ReportOf(scores []CaseScore) Report {
	byTag := make(map[string][]CaseScore)
	byFamily := make(map[string][]CaseScore)
	for i := range scores {
		for _, tag := range scores[i].Tags {
			byTag[tag] = append(byTag[tag], scores[i])
		}
		for _, family := range scores[i].SourceFamilies {
			byFamily[family] = append(byFamily[family], scores[i])
		}
	}
	return Report{
		Cases:          len(scores),
		Overall:        Aggregate(scores),
		ByTag:          groupReport(byTag),
		BySourceFamily: groupReport(byFamily),
	}
}

func groupReport(groups map[string][]CaseScore) GroupReport {
	out := GroupReport{Groups: make(map[string]Metrics, len(groups))}
	for name, members := range groups {
		out.Groups[name] = Aggregate(members)
	}
	out.Macro = MacroOf(out.Groups)
	return out
}

// Aggregate averages each rate over the cases where it is defined, and
// summarizes cold and warm latency separately.
func Aggregate(scores []CaseScore) Metrics {
	m := Metrics{Cases: len(scores)}
	for _, rf := range rateFields() {
		sum, n := 0.0, 0
		for i := range scores {
			if v := rf.score(&scores[i]); v.OK {
				sum += v.V
				n++
			}
		}
		if n > 0 {
			*rf.mean(&m.Rates) = Mean{Value: sum / float64(n), N: n}
		}
	}

	var cold, warm []time.Duration
	for i := range scores {
		if scores[i].Cold {
			cold = append(cold, scores[i].Latency)
		} else {
			warm = append(warm, scores[i].Latency)
		}
	}
	m.Latency = LatencyStats{Cold: percentilesOf(cold), Warm: percentilesOf(warm)}
	return m
}

func percentilesOf(d []time.Duration) Percentiles {
	p := Percentiles{N: len(d)}
	p.P50, _ = Percentile(d, 50)
	p.P95, _ = Percentile(d, 95)
	return p
}

// MacroOf averages each rate across groups, weighting every group equally.
//
// This is the number that makes a small group's regression visible: a
// thousand-case document group and a three-case exact-lookup group each move
// the macro average by the same amount, so a collapse in the small group
// cannot be absorbed by the large one.
//
// Groups are visited in sorted order so a report is byte-identical between
// runs over the same data.
func MacroOf(groups map[string]Metrics) Macro {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	out := Macro{Groups: len(groups)}
	for _, rf := range rateFields() {
		sum, n := 0.0, 0
		for _, name := range names {
			g := groups[name]
			if mean := rf.mean(&g.Rates); mean.Defined() {
				sum += mean.Value
				n++
			}
		}
		if n > 0 {
			*rf.mean(&out.Rates) = Mean{Value: sum / float64(n), N: n}
		}
	}
	return out
}

// sortedRoots orders a root-keyed assertion map, so a case with two failures
// reports them in the same order on every run.
func sortedRoots(m map[recall.LineageRoot]string) []recall.LineageRoot {
	out := make([]recall.LineageRoot, 0, len(m))
	for root := range m {
		out = append(out, root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
