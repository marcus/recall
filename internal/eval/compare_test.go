package eval_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/eval"
)

func ndcgRates(value float64) eval.Rates {
	return eval.Rates{NDCG10: eval.Mean{Value: value, N: 1}}
}

func ndcgRatesN(value float64, n int) eval.Rates {
	return eval.Rates{NDCG10: eval.Mean{Value: value, N: n}}
}

func comparisonRunWithFamilyNDCG(id string, familyNDCG float64) eval.Run {
	const stable = 0.8
	return eval.Run{
		RunID:  id,
		Status: eval.StatusPass,
		Pack:   eval.PackRef{ContentHash: "sha256:same"},
		Metrics: eval.Report{
			Overall: eval.Metrics{Rates: ndcgRates(stable)},
			ByTag: eval.GroupReport{
				Groups: map[string]eval.Metrics{
					"shared": {Rates: ndcgRates(stable)},
				},
				Macro: eval.Macro{Rates: ndcgRates(stable), Groups: 1},
			},
			BySourceFamily: eval.GroupReport{
				Groups: map[string]eval.Metrics{
					"shared": {Rates: ndcgRates(familyNDCG)},
				},
				Macro: eval.Macro{Rates: ndcgRates(familyNDCG), Groups: 1},
			},
		},
	}
}

func TestSourceFamilyRegressionCannotHideBehindOverallAndTags(t *testing.T) {
	baseline := comparisonRunWithFamilyNDCG("baseline", 0.8)
	candidate := comparisonRunWithFamilyNDCG("candidate", 0.7)

	got := eval.Compare(baseline, candidate, nil, nil)
	if got.Acceptable() {
		t.Fatal("a source-family-only loss left the comparison acceptable")
	}
	if len(got.Regressions) != 2 {
		t.Fatalf("regressions = %v, want source-family group and macro losses",
			got.Regressions)
	}
	for _, regression := range got.Regressions {
		if regression.Dimension != "source_family" || regression.Metric != "ndcg_at_10" {
			t.Errorf("unexpected regression: %+v", regression)
		}
	}

	var tagKey, familyKey string
	for _, delta := range got.ByTag {
		if delta.Metric == "ndcg_at_10" && delta.Population == "group" {
			tagKey = delta.Key
		}
	}
	for _, delta := range got.BySourceFamily {
		if delta.Metric == "ndcg_at_10" && delta.Population == "group" {
			familyKey = delta.Key
		}
	}
	if tagKey != "tag:group:shared" || familyKey != "source_family:group:shared" {
		t.Fatalf("dimension-qualified keys = %q and %q", tagKey, familyKey)
	}
	if tagKey == familyKey {
		t.Fatal("same-named tag and source family collided")
	}

	human := eval.SummarizeComparison(got)
	for _, want := range []string{
		`source family group "shared"/ndcg_at_10`,
		"source family macro/ndcg_at_10",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("human output does not identify %q:\n%s", want, human)
		}
	}
}

func familyPopulationPair(keepUndefinedGroup bool) (eval.Run, eval.Run) {
	baseline := comparisonRunWithFamilyNDCG("baseline", 0.8)
	candidate := comparisonRunWithFamilyNDCG("candidate", 0.8)

	baseline.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{
			"lost":   {Rates: ndcgRatesN(0.8, 2), Cases: 2},
			"stable": {Rates: ndcgRates(0.8), Cases: 1},
		},
		Macro: eval.Macro{Rates: ndcgRatesN(0.8, 2), Groups: 2},
	}
	candidateGroups := map[string]eval.Metrics{
		"stable": {Rates: ndcgRates(0.8), Cases: 1},
	}
	if keepUndefinedGroup {
		candidateGroups["lost"] = eval.Metrics{Cases: 2}
	}
	candidate.Metrics.BySourceFamily = eval.GroupReport{
		Groups: candidateGroups,
		Macro:  eval.Macro{Rates: ndcgRates(0.8), Groups: len(candidateGroups)},
	}
	return baseline, candidate
}

func TestSourceFamilyPopulationLossIsARegression(t *testing.T) {
	for _, tc := range []struct {
		name               string
		keepUndefinedGroup bool
	}{
		{name: "group absent", keepUndefinedGroup: false},
		{name: "group present with metric N zero", keepUndefinedGroup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline, candidate := familyPopulationPair(tc.keepUndefinedGroup)
			got := eval.Compare(baseline, candidate, nil, nil)
			if got.Acceptable() {
				t.Fatal("a measured source family disappeared without failing comparison")
			}
			if len(got.Regressions) != 1 {
				t.Fatalf("regressions = %+v, want only the lost family metric",
					got.Regressions)
			}
			regression := got.Regressions[0]
			if regression.Key != "source_family:group:lost" ||
				regression.BaselineN != 2 ||
				regression.CandidateN != 0 ||
				regression.Defined {
				t.Fatalf("population-loss regression = %+v", regression)
			}
			human := eval.SummarizeComparison(got)
			if !strings.Contains(human, "0.8000 (n=2) → n/a (n=0); population disappeared") {
				t.Errorf("human output hides the two populations:\n%s", human)
			}
		})
	}
}

func TestAllSourceFamiliesAndMacroDisappearingIsARegression(t *testing.T) {
	baseline := comparisonRunWithFamilyNDCG("baseline", 0.8)
	candidate := comparisonRunWithFamilyNDCG("candidate", 0.8)
	candidate.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{},
	}

	got := eval.Compare(baseline, candidate, nil, nil)
	if got.Acceptable() {
		t.Fatal("all source families disappeared without failing comparison")
	}
	if len(got.Regressions) != 2 {
		t.Fatalf("regressions = %+v, want missing family group and macro",
			got.Regressions)
	}
	want := []string{"source_family:group:shared", "source_family:macro"}
	for i, regression := range got.Regressions {
		if regression.Key != want[i] ||
			regression.BaselineN != 1 ||
			regression.CandidateN != 0 {
			t.Errorf("regression %d = %+v, want key %s with n=1 to n=0",
				i, regression, want[i])
		}
	}
}

func TestNewlyDefinedSourceFamilyIsNotClaimedAsImprovement(t *testing.T) {
	baseline := comparisonRunWithFamilyNDCG("baseline", 0.8)
	candidate := comparisonRunWithFamilyNDCG("candidate", 0.8)
	baseline.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{"new": {}},
	}
	candidate.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{
			"new": {Rates: ndcgRates(0.8), Cases: 1},
		},
		Macro: eval.Macro{Rates: ndcgRates(0.8), Groups: 1},
	}

	got := eval.Compare(baseline, candidate, nil, nil)
	if !got.Acceptable() {
		t.Fatalf("undefined-to-defined was treated as a loss: %+v", got.Regressions)
	}
	for _, delta := range got.BySourceFamily {
		if delta.Metric != "ndcg_at_10" || delta.Population != "group" {
			continue
		}
		if delta.BaselineN != 0 || delta.CandidateN != 1 ||
			delta.Defined || delta.Change != 0 {
			t.Fatalf("new population was claimed as a numeric improvement: %+v", delta)
		}
		return
	}
	t.Fatal("new source-family delta was not emitted")
}

func comparisonRunWithGroupOrder(id string, names []string) eval.Run {
	tagGroups := make(map[string]eval.Metrics, len(names))
	familyGroups := make(map[string]eval.Metrics, len(names))
	for _, name := range names {
		tagGroups[name] = eval.Metrics{Rates: ndcgRates(0.8)}
		familyGroups[name] = eval.Metrics{Rates: ndcgRates(0.8)}
	}
	return eval.Run{
		RunID:  id,
		Status: eval.StatusPass,
		Pack:   eval.PackRef{ContentHash: "sha256:same"},
		Metrics: eval.Report{
			ByTag: eval.GroupReport{
				Groups: tagGroups,
				Macro:  eval.Macro{Rates: ndcgRates(0.8), Groups: len(names)},
			},
			BySourceFamily: eval.GroupReport{
				Groups: familyGroups,
				Macro:  eval.Macro{Rates: ndcgRates(0.8), Groups: len(names)},
			},
		},
	}
}

func uniqueDeltaKeys(deltas []eval.Delta) []string {
	var out []string
	for _, delta := range deltas {
		if len(out) == 0 || out[len(out)-1] != delta.Key {
			out = append(out, delta.Key)
		}
	}
	return out
}

func TestComparisonGroupAndMacroOrderingIsDeterministic(t *testing.T) {
	forward := eval.Compare(
		comparisonRunWithGroupOrder("baseline", []string{"zeta", "alpha"}),
		comparisonRunWithGroupOrder("candidate", []string{"alpha", "zeta"}),
		nil,
		nil,
	)
	reverse := eval.Compare(
		comparisonRunWithGroupOrder("baseline", []string{"alpha", "zeta"}),
		comparisonRunWithGroupOrder("candidate", []string{"zeta", "alpha"}),
		nil,
		nil,
	)

	wantTags := []string{"tag:group:alpha", "tag:group:zeta"}
	wantFamilies := []string{
		"source_family:group:alpha",
		"source_family:group:zeta",
		"source_family:macro",
	}
	if got := uniqueDeltaKeys(forward.ByTag); !reflect.DeepEqual(got, wantTags) {
		t.Errorf("tag order = %v, want %v", got, wantTags)
	}
	if got := uniqueDeltaKeys(forward.BySourceFamily); !reflect.DeepEqual(got, wantFamilies) {
		t.Errorf("source-family order = %v, want %v", got, wantFamilies)
	}
	wantMetrics := len(forward.Overall)
	for _, deltas := range [][]eval.Delta{forward.ByTag, forward.BySourceFamily} {
		counts := map[string]int{}
		for _, delta := range deltas {
			counts[delta.Key]++
		}
		for key, count := range counts {
			if count != wantMetrics {
				t.Errorf("%s emitted %d metrics, want every one of %d",
					key, count, wantMetrics)
			}
		}
	}

	forwardJSON, err := json.Marshal(forward)
	if err != nil {
		t.Fatal(err)
	}
	reverseJSON, err := json.Marshal(reverse)
	if err != nil {
		t.Fatal(err)
	}
	if string(forwardJSON) != string(reverseJSON) {
		t.Errorf("map insertion order changed comparison JSON:\n%s\n%s",
			forwardJSON, reverseJSON)
	}
}
