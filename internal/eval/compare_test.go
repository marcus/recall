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

	wantTags := []string{"tag:group:alpha", "tag:group:zeta", "tag:macro"}
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
