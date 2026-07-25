package eval

import (
	"fmt"
	"sort"
	"time"

	"github.com/marcus/recall/internal/recall"
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
	// Locator is the reference the run returned.
	Locator recall.Locator `json:"locator"`

	// Root is the lineage root the expansion resolved to, empty when it did not
	// resolve at all.
	Root recall.LineageRoot `json:"lineage_root,omitempty"`

	// Revision is the source revision the expansion landed on. Resolving to the
	// right record at the wrong revision is a locator failure, not a near miss.
	Revision string `json:"source_revision,omitempty"`
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

	// SourceFamilies names the source families that contributed to this
	// result. A case belongs to every family it touched, so a family's metrics
	// cover every case it had a hand in.
	SourceFamilies []string `json:"source_families,omitempty"`

	// Behavior is what Recall actually did: answered, asked to clarify, or
	// abstained.
	Behavior Behavior `json:"behavior"`

	// Coverage is what the response reported. The truth it is checked against
	// comes from the case's assertions, never from the response.
	Coverage recall.Coverage `json:"coverage"`

	// SourceOutcomes is what each source reported for this case.
	SourceOutcomes map[recall.SourceUID]recall.SearchOutcome `json:"source_outcomes,omitempty"`

	Expansions []Expansion  `json:"expansions,omitempty"`
	Provenance []Provenance `json:"provenance,omitempty"`

	Latency time.Duration `json:"latency_ns"`

	// Cold says whether this case ran against cold caches. Cold and warm
	// latency are never pooled: their distributions are different questions.
	Cold bool `json:"cold"`
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
		CaseID:         c.CaseID,
		Tags:           c.Tags,
		SourceFamilies: r.SourceFamilies,
		Latency:        r.Latency,
		Cold:           r.Cold,
	}

	s.NDCG10 = value(NDCGAt(r.Ranked, grades, ndcgK))
	s.Recall5 = value(RecallAt(r.Ranked, required, recallK))
	s.Recall20 = value(RecallAt(r.Ranked, required, recallDeepK))
	s.MRR10 = value(MRRAt(r.Ranked, authoritative, mrrK))

	// Success is measured against evidence the pack says is worth finding. A
	// case with nothing useful to find cannot succeed or fail at finding it.
	if len(useful) > 0 {
		s.Success5 = boolValue(HitAt(r.Ranked, useful, successK))
	}
	// Likewise, a case that names no forbidden evidence was never at risk of
	// surfacing any, and counting it as a clean pass would dilute the rate.
	if len(forbidden) > 0 {
		s.Forbidden5 = boolValue(HitAt(r.Ranked, forbidden, forbiddenK))
	}

	if c.ExpectedBehavior.Valid() {
		s.AbstentionAccuracy = boolValue(r.Behavior == c.ExpectedBehavior)
	}
	s.CoverageAccuracy = coverageAccuracy(c, r)
	s.LocatorSuccess = locatorSuccess(c, r)
	s.ProvenanceAccuracy = provenanceAccuracy(r)
	s.SourceOutcomeAccuracy = sourceOutcomeAccuracy(c, r)

	return s
}

func value(v float64, ok bool) Value { return Value{V: v, OK: ok} }

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
