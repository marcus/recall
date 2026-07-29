package eval_test

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/recall"
)

// scored builds a case score carrying one metric, which is all the grouping
// and averaging tests need.
func scored(id, tag string, ndcg float64) eval.CaseScore {
	return eval.CaseScore{
		CaseID: id,
		Tags:   []string{tag},
		NDCG10: eval.Value{V: ndcg, OK: true},
	}
}

// The reason macro averaging exists, made explicit.
//
// A run holds one hundred document cases and two exact-lookup cases. A change
// destroys exact lookup completely and leaves documents untouched. Pooled over
// every case the damage is under two points, which is inside anyone's run
// variance and would be waved through. The macro average, which counts each
// group once, shows it as the halving it is.
func TestMacroAveragingCannotLetALargeGroupMaskASmallOne(t *testing.T) {
	const bulk = 100

	var baseline, candidate []eval.CaseScore
	for i := range bulk {
		id := "docs-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		baseline = append(baseline, scored(id, "documents", 1.0))
		candidate = append(candidate, scored(id, "documents", 1.0))
	}
	for _, id := range []string{"exact-1", "exact-2"} {
		baseline = append(baseline, scored(id, "exact", 1.0))
		candidate = append(candidate, scored(id, "exact", 0.0))
	}

	before := eval.ReportOf(baseline)
	after := eval.ReportOf(candidate)

	// What a pooled average sees: a rounding error.
	pooledDrop := before.Overall.NDCG10.Value - after.Overall.NDCG10.Value
	closeTo(t, "pooled nDCG@10 drop", pooledDrop, 2.0/102.0)
	if pooledDrop > 0.02 {
		t.Fatalf("pooled drop %.4f is not small enough for this test to mean anything", pooledDrop)
	}

	// What the macro average sees: half the retrieval quality gone.
	macroDrop := before.ByTag.Macro.NDCG10.Value - after.ByTag.Macro.NDCG10.Value
	closeTo(t, "macro nDCG@10 drop", macroDrop, 0.5)

	if macroDrop <= pooledDrop*10 {
		t.Errorf("macro drop %.4f did not expose what the pooled drop %.4f hid", macroDrop, pooledDrop)
	}

	// And the group itself is unambiguous, so a report reader can find it.
	if got := after.ByTag.Groups["exact"]; got.NDCG10.Value != 0 || got.NDCG10.N != 2 {
		t.Errorf("exact group = %+v, want nDCG 0 over 2 cases", got.NDCG10)
	}
	if got := after.ByTag.Groups["documents"]; got.NDCG10.Value != 1 {
		t.Errorf("documents group = %+v, want an untouched 1.0", got.NDCG10)
	}
	// Both groups weigh the same in the macro average, whatever their size.
	if after.ByTag.Macro.NDCG10.N != 2 {
		t.Errorf("macro averaged over %d groups, want 2", after.ByTag.Macro.NDCG10.N)
	}
}

// A case belongs to every tag it carries, so a case tagged both "exact" and
// "temporal" is measured in both groups rather than being assigned to one.
func TestACaseJoinsEveryGroupItDeclares(t *testing.T) {
	s := eval.CaseScore{
		CaseID:         "multi",
		Tags:           []string{"exact", "temporal"},
		SourceFamilies: []string{"tasks", "documents"},
		NDCG10:         eval.Value{V: 0.5, OK: true},
	}
	r := eval.ReportOf([]eval.CaseScore{s})

	for _, tag := range []string{"exact", "temporal"} {
		if got := r.ByTag.Groups[tag].NDCG10.N; got != 1 {
			t.Errorf("tag %q covered %d cases, want 1", tag, got)
		}
	}
	for _, family := range []string{"tasks", "documents"} {
		if got := r.BySourceFamily.Groups[family].NDCG10.N; got != 1 {
			t.Errorf("family %q covered %d cases, want 1", family, got)
		}
	}
	if r.Cases != 1 {
		t.Errorf("cases = %d, want 1: grouping must not multiply the run", r.Cases)
	}
}

// An undefined metric is skipped, not counted as zero. Counting it would let a
// pack of abstention cases, which have no forbidden evidence to surface, drive
// Forbidden@5 to a reassuring zero it never earned.
func TestAggregateSkipsUndefinedRatherThanCountingZero(t *testing.T) {
	scores := []eval.CaseScore{
		{CaseID: "a", Forbidden5: eval.Value{V: 1, OK: true}},
		{CaseID: "b"}, // no forbidden judgments: undefined
		{CaseID: "c"}, // likewise
	}
	m := eval.Aggregate(scores)

	if m.Forbidden5.N != 1 {
		t.Fatalf("averaged over %d cases, want the 1 where it applied", m.Forbidden5.N)
	}
	closeTo(t, "Forbidden@5", m.Forbidden5.Value, 1.0)
	if m.Cases != 3 {
		t.Errorf("cases = %d, want all 3 counted in the population", m.Cases)
	}
}

// A metric no case defined has no value at all. Reporting 0.0 with n=0 as if
// it were a measurement is how an absence of evidence becomes a regression.
func TestAMetricNoCaseDefinedIsNotAZero(t *testing.T) {
	m := eval.Aggregate([]eval.CaseScore{{CaseID: "a"}, {CaseID: "b"}})
	if m.NDCG10.Defined() {
		t.Errorf("nDCG@10 = %+v, want undefined", m.NDCG10)
	}
}

// A group where the metric never applied must not pull the macro average
// toward zero; it simply does not vote.
func TestMacroSkipsGroupsWhereTheMetricNeverApplied(t *testing.T) {
	groups := map[string]eval.Metrics{
		"graded":   {Rates: eval.Rates{NDCG10: eval.Mean{Value: 0.8, N: 4}}},
		"abstains": {Rates: eval.Rates{}},
	}
	macro := eval.MacroOf(groups)

	if macro.Groups != 2 {
		t.Errorf("groups = %d, want 2", macro.Groups)
	}
	if macro.NDCG10.N != 1 {
		t.Fatalf("nDCG averaged over %d groups, want only the one that measured it", macro.NDCG10.N)
	}
	closeTo(t, "macro nDCG@10", macro.NDCG10.Value, 0.8)
}

// Cold and warm are different distributions and are never pooled: a cold start
// answers "how long until this is usable", a warm one "how long does a query
// take", and one number for both answers neither.
func TestLatencyKeepsColdAndWarmApart(t *testing.T) {
	var scores []eval.CaseScore
	for i := 1; i <= 20; i++ {
		scores = append(scores,
			eval.CaseScore{CaseID: "w", Latency: time.Duration(i) * time.Millisecond},
			eval.CaseScore{CaseID: "c", Cold: true, Latency: time.Duration(100*i) * time.Millisecond},
		)
	}
	m := eval.Aggregate(scores)

	if m.Latency.Warm.N != 20 || m.Latency.Cold.N != 20 {
		t.Fatalf("split cold %d / warm %d, want 20 each", m.Latency.Cold.N, m.Latency.Warm.N)
	}
	// Nearest rank over 20 samples: p50 is the 10th, p95 the 19th.
	if got, want := m.Latency.Warm.P50, 10*time.Millisecond; got != want {
		t.Errorf("warm p50 = %v, want %v", got, want)
	}
	if got, want := m.Latency.Warm.P95, 19*time.Millisecond; got != want {
		t.Errorf("warm p95 = %v, want %v", got, want)
	}
	if got, want := m.Latency.Cold.P95, 1900*time.Millisecond; got != want {
		t.Errorf("cold p95 = %v, want %v", got, want)
	}
}

// A metric added to CaseScore but never aggregated is invisible: it would be
// computed for every case and reported by nothing. This walks the types rather
// than listing them, so adding a metric without wiring it up fails here.
func TestEveryRateIsAggregated(t *testing.T) {
	valueType := reflect.TypeOf(eval.Value{})
	meanType := reflect.TypeOf(eval.Mean{})

	score := eval.CaseScore{CaseID: "a"}
	sv := reflect.ValueOf(&score).Elem()
	perCase := 0
	for i := range sv.NumField() {
		if sv.Type().Field(i).Type != valueType {
			continue
		}
		perCase++
		sv.Field(i).Set(reflect.ValueOf(eval.Value{V: 0.5, OK: true}))
	}

	m := eval.Aggregate([]eval.CaseScore{score})
	rates := reflect.ValueOf(m.Rates)
	aggregated := 0
	for i := range rates.NumField() {
		f := rates.Type().Field(i)
		if f.Type != meanType {
			t.Fatalf("Rates.%s is %s, want %s", f.Name, f.Type, meanType)
		}
		aggregated++
		got, ok := rates.Field(i).Interface().(eval.Mean)
		if !ok {
			t.Fatalf("Rates.%s did not read back as a Mean", f.Name)
		}
		if got.N != 1 || math.Abs(got.Value-0.5) > tolerance {
			t.Errorf("Rates.%s = %+v, want 0.5 over 1 case: it is not being aggregated", f.Name, got)
		}
	}

	if perCase != aggregated {
		t.Errorf("CaseScore carries %d metrics, Rates reports %d", perCase, aggregated)
	}
}

// Scoring reads judgments for the case being scored and ignores the rest, so a
// caller cannot get a wrong answer by handing over the whole pack.
func TestScoreIgnoresAnotherCasesJudgments(t *testing.T) {
	c := eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           "mine",
		Query:            "q",
		Profile:          "smoke",
		ExpectedBehavior: eval.BehaviorAnswer,
	}
	judgments := []eval.Judgment{
		{SchemaVersion: 1, CaseID: "mine", LineageRoot: root("a"), Relevance: eval.Authoritative, Required: true},
		{SchemaVersion: 1, CaseID: "theirs", LineageRoot: root("b"), Relevance: eval.Authoritative, Required: true},
	}
	result := eval.CaseResult{CaseID: "mine", Ranked: roots("a"), Behavior: eval.BehaviorAnswer}

	s := eval.Score(c, judgments, result)
	if !s.Recall5.OK {
		t.Fatal("Recall@5 undefined although this case required evidence")
	}
	closeTo(t, "Recall@5", s.Recall5.V, 1.0)
	closeTo(t, "MRR@10", s.MRR10.V, 1.0)
}

// The four policy metrics, each measured against what the case declared rather
// than against what the response said about itself.
func TestScorePolicyMetrics(t *testing.T) {
	c := eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           "policy",
		Query:            "q",
		Profile:          "smoke",
		ExpectedBehavior: eval.BehaviorAbstain,
		Assertions: &eval.Assertions{
			ExpectedCoverage: recall.CoverageDegraded,
			ExpectedSourceOutcomes: map[recall.SourceUID]recall.SearchOutcome{
				"uid-tasks": recall.SearchTimeout,
				"uid-docs":  recall.SearchDenied,
			},
			ExpectedRevisions: map[recall.LineageRoot]string{root("a"): "rev-7"},
		},
	}
	result := eval.CaseResult{
		CaseID:   "policy",
		Behavior: eval.BehaviorAbstain,
		Coverage: recall.CoverageDegraded,
		SourceOutcomes: map[recall.SourceUID]recall.SearchOutcome{
			"uid-tasks": recall.SearchTimeout,
			"uid-docs":  recall.SearchSuccess, // dishonest: the fixture denied it
		},
		Expansions: []eval.Expansion{
			{Root: root("a"), Revision: "rev-7"}, // right record, right revision
			{Root: root("a"), Revision: "rev-6"}, // right record, stale revision
			{Root: ""},                           // did not resolve at all
			{Root: root("z"), Revision: "any"},   // unjudged record, but it resolved
		},
		Provenance: []eval.Provenance{
			{ClaimedSource: "uid-tasks", ActualSource: "uid-tasks", ClaimedRoot: root("a"), ActualRoot: root("a")},
			{ClaimedSource: "uid-tasks", ActualSource: "uid-docs", ClaimedRoot: root("a"), ActualRoot: root("a")},
		},
	}

	s := eval.Score(c, nil, result)

	closeTo(t, "abstention accuracy", s.AbstentionAccuracy.V, 1.0)
	closeTo(t, "coverage accuracy", s.CoverageAccuracy.V, 1.0)
	// Half the sources were reported honestly.
	closeTo(t, "source-outcome accuracy", s.SourceOutcomeAccuracy.V, 0.5)
	// Two of four references are good: the stale revision and the unresolvable
	// reference both fail.
	closeTo(t, "locator success", s.LocatorSuccess.V, 0.5)
	closeTo(t, "provenance accuracy", s.ProvenanceAccuracy.V, 0.5)
}

// Reporting degradation that did not happen is as wrong as hiding degradation
// that did. Both directions teach a caller to ignore the signal.
func TestCoverageAccuracyPunishesBothDirections(t *testing.T) {
	cases := []struct {
		name             string
		expected, actual recall.Coverage
		want             float64
	}{
		{"degraded and true", recall.CoverageDegraded, recall.CoverageDegraded, 1},
		{"complete and true", recall.CoverageComplete, recall.CoverageComplete, 1},
		{"hid a degradation", recall.CoverageDegraded, recall.CoverageComplete, 0},
		{"invented a degradation", recall.CoverageComplete, recall.CoverageDegraded, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := eval.Case{
				CaseID:           "c",
				ExpectedBehavior: eval.BehaviorAnswer,
				Assertions:       &eval.Assertions{ExpectedCoverage: tc.expected},
			}
			s := eval.Score(c, nil, eval.CaseResult{CaseID: "c", Coverage: tc.actual})
			if !s.CoverageAccuracy.OK {
				t.Fatal("undefined although the case declared expected coverage")
			}
			closeTo(t, "coverage accuracy", s.CoverageAccuracy.V, tc.want)
		})
	}
}

// A case that declares no coverage expectation has no ground truth to be
// scored against, and guessing one would manufacture a metric.
func TestCoverageAccuracyUndefinedWithoutAnAssertion(t *testing.T) {
	c := eval.Case{CaseID: "c", ExpectedBehavior: eval.BehaviorAnswer}
	s := eval.Score(c, nil, eval.CaseResult{CaseID: "c", Coverage: recall.CoverageComplete})
	if s.CoverageAccuracy.OK {
		t.Error("coverage accuracy defined without an expectation to check")
	}
}

// Success and Forbidden are scored only for cases that could have succeeded or
// failed at them. A pack of abstention cases has no useful evidence to find
// and no forbidden evidence to surface; scoring those zero and one respectively
// would report a collapse and a clean sheet that neither were earned.
func TestSuccessAndForbiddenApplyOnlyWhereThePackPutSomethingAtStake(t *testing.T) {
	c := goodCase("c")
	result := eval.CaseResult{CaseID: "c", Ranked: roots("a", "b"), Behavior: eval.BehaviorAnswer}

	bare := eval.Score(c, nil, result)
	if bare.Success5.OK {
		t.Error("Success@5 defined although the pack judged nothing useful")
	}
	if bare.Forbidden5.OK {
		t.Error("Forbidden@5 defined although the pack forbade nothing")
	}

	judged := eval.Score(c, []eval.Judgment{
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("a"), Relevance: eval.UsefulSupport},
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("b"), Relevance: eval.Irrelevant, Forbidden: true},
	}, result)
	if !judged.Success5.OK || judged.Success5.V != 1 {
		t.Errorf("Success@5 = %+v, want a hit", judged.Success5)
	}
	if !judged.Forbidden5.OK || judged.Forbidden5.V != 1 {
		t.Errorf("Forbidden@5 = %+v, want the forbidden root reported", judged.Forbidden5)
	}

	// Related context alone is not useful support: grade 1 is the floor for
	// "worth reading", not for "answers the question".
	related := eval.Score(c, []eval.Judgment{
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("a"), Relevance: eval.RelatedContext},
	}, result)
	if related.Success5.OK {
		t.Error("Success@5 defined for a case whose best evidence is related context")
	}
}

// A result naming a case the pack does not hold means the runner and the pack
// disagree about what was run. Every number after that is untrustworthy, so it
// is an error rather than a skipped row.
func TestScoresRejectsAResultForAnUnknownCase(t *testing.T) {
	_, err := eval.Scores(nil, nil, []eval.CaseResult{{CaseID: "ghost"}})
	if err == nil {
		t.Fatal("accepted a result for a case the pack does not contain")
	}
}

// A record reached through two source instances takes one result slot, and the
// response says so. Scoring both of its roots as separate positions credits the
// answer twice for one hit and measures everything behind it at a rank no
// caller ever saw — which is how a fix that removed a duplicate read as a
// ranking regression.
func TestFusedViewsAreOneRankPosition(t *testing.T) {
	c := eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           "c",
		Query:            "q",
		Profile:          "smoke",
		ExpectedBehavior: eval.BehaviorAnswer,
	}
	// Both views of the project are judged, as a pack grading evidence rather
	// than results would write them, and so is a document further down.
	judgments := []eval.Judgment{
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("project"), Relevance: eval.Authoritative, Required: true},
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("project-view"), Relevance: eval.Authoritative, Required: true},
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("doc"), Relevance: eval.Authoritative, Required: true},
	}
	ranked := roots("project", "project-view", "filler-1", "filler-2", "filler-3", "doc")

	fused := eval.Score(c, judgments, eval.CaseResult{
		CaseID:   "c",
		Ranked:   ranked,
		Behavior: eval.BehaviorAnswer,
		Suppressions: []recall.Suppression{{
			Reason:      recall.SuppressDuplicateView,
			Count:       1,
			LineageRoot: root("project-view"),
			FusedInto:   root("project"),
		}},
	})
	// Two records to find, both inside the caller's first five slots.
	closeTo(t, "Recall@5", fused.Recall5.V, 1.0)

	// Without the response saying they are one record, the document is scored
	// at position six and the case looks like a miss.
	split := eval.Score(c, judgments, eval.CaseResult{
		CaseID: "c", Ranked: ranked, Behavior: eval.BehaviorAnswer,
	})
	closeTo(t, "Recall@5 unfused", split.Recall5.V, 2.0/3.0)

	// Graded gain is the other half: a second view earns none of its own. The
	// fused pair scores exactly what the same answer scores with the view never
	// returned and never judged, which is lower than the split number above —
	// that one was counting one record as two relevant hits, one of them at
	// position two.
	deduped := eval.Score(c, []eval.Judgment{
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("project"), Relevance: eval.Authoritative, Required: true},
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("doc"), Relevance: eval.Authoritative, Required: true},
	}, eval.CaseResult{
		CaseID:   "c",
		Ranked:   roots("project", "filler-1", "filler-2", "filler-3", "doc"),
		Behavior: eval.BehaviorAnswer,
	})
	closeTo(t, "nDCG@10", fused.NDCG10.V, deduped.NDCG10.V)
	closeTo(t, "MRR@10", fused.MRR10.V, deduped.MRR10.V)
}

// The same mapping applies to evidence a pack forbids: a forbidden record
// folded into a displayed result was still displayed, and reading the roots
// literally would let fusion hide it.
func TestFusedViewsCannotHideForbiddenEvidence(t *testing.T) {
	c := eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           "c",
		Query:            "q",
		Profile:          "smoke",
		ExpectedBehavior: eval.BehaviorAnswer,
	}
	judgments := []eval.Judgment{
		{SchemaVersion: 1, CaseID: "c", LineageRoot: root("secret-view"), Forbidden: true},
	}
	s := eval.Score(c, judgments, eval.CaseResult{
		CaseID:   "c",
		Ranked:   roots("shown"),
		Behavior: eval.BehaviorAnswer,
		Suppressions: []recall.Suppression{{
			Reason:      recall.SuppressDuplicateView,
			Count:       1,
			LineageRoot: root("secret-view"),
			FusedInto:   root("shown"),
		}},
	})
	if !s.Forbidden5.OK || s.Forbidden5.V != 1 {
		t.Errorf("Forbidden@5 = %+v, want a hit: the record was displayed under the other view", s.Forbidden5)
	}
}
