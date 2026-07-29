package explain_test

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/explain"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/pkg/recall"
)

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

// full is an explanation with every field populated, so the coverage test below
// has something to be exhaustive about.
func full() recall.Explanation {
	return recall.Explanation{
		SourceUID:     "uid-people",
		SourceID:      "people",
		LocalRank:     1,
		LocalPoolSize: 8,
		MatchSignals:  []recall.MatchSignal{recall.MatchExactIdentifier, recall.MatchAlias},
		Prior: recall.PriorExplanation{
			Base: 1.0, Intent: 0.25, Rule: "person_lookup", Effective: 1.25,
		},
		LineageRoot: "uid-people:p-42",
		Corroboration: recall.CorroborationExplanation{
			IndependentUnits: 2,
			Sources:          []string{"tasks", "clara-signals"},
			Cap:              2.0,
			CapApplied:       true,
		},
		Freshness: recall.FreshnessExplanation{
			Mode:            recall.FreshnessIndexed,
			SourceRevision:  "cursor-4412",
			IndexGeneration: "gen-000017",
			IndexModel:      "bge-small-en-v1.5",
			IndexConfig:     "bm25-k1.2-b0.75",
			ObservedAt:      ts("2026-07-23T12:00:00Z"),
			ConfirmedAt:     ts("2026-07-23T12:00:05Z"),
			AsOfHonored:     recall.AsOfFilter,
		},
		Reranker: recall.RerankerExplanation{
			Used: true, Model: "local-cross-encoder", Delta: 0.02, RankBefore: 4,
		},
		ExactPromoted: true,
		Score:         0.0328,
		RankConstant:  60,
	}
}

// boolMarkers name the text that stands for a true boolean, since a bool has no
// value to search for.
var boolMarkers = map[string]string{
	"Corroboration.CapApplied": "applied",
	"Reranker.Used":            "used",
	"ExactPromoted":            "promoted",
}

// A field that exists in the explanation but never reaches the rendering is
// invisible, and an invisible policy is indistinguishable from one that never
// applied. Walking the struct means adding a field fails this test rather than
// going quietly missing.
func TestRenderCoversEveryExplanationField(t *testing.T) {
	out := explain.Render(full())
	t.Logf("rendered:\n%s", out)

	var walk func(v reflect.Value, path string)
	walk = func(v reflect.Value, path string) {
		typ := v.Type()
		for i := range v.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			fv := v.Field(i)
			name := field.Name
			if path != "" {
				name = path + "." + name
			}

			if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeOf(time.Time{}) {
				walk(fv, name)
				continue
			}
			for _, want := range expected(name, fv) {
				if !strings.Contains(out, want) {
					t.Errorf("%s is not visible in the rendering (looked for %q)", name, want)
				}
			}
		}
	}
	walk(reflect.ValueOf(full()), "")
}

// expected returns the substrings that prove a field reached the output.
func expected(name string, v reflect.Value) []string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Bool:
		if !v.Bool() {
			return nil
		}
		if marker, ok := boolMarkers[name]; ok {
			return []string{marker}
		}
		return []string{name}
	case reflect.String:
		if v.String() == "" {
			return nil
		}
		return []string{v.String()}
	case reflect.Int, reflect.Int64:
		if v.Int() == 0 {
			return nil
		}
		return []string{strconv.FormatInt(v.Int(), 10)}
	case reflect.Float64:
		if v.Float() == 0 {
			return nil
		}
		return []string{strconv.FormatFloat(v.Float(), 'g', 6, 64)}
	case reflect.Slice:
		var out []string
		for i := range v.Len() {
			out = append(out, expected(name, v.Index(i))...)
		}
		return out
	case reflect.Struct: // time.Time
		if t, ok := v.Interface().(time.Time); ok && !t.IsZero() {
			return []string{t.UTC().Format(time.RFC3339)}
		}
		return nil
	default:
		return nil
	}
}

// The invariant this package exists to enforce: every configured value that
// moved a result is readable off that result. This drives real fusion rather
// than a hand-built explanation, so a knob that ranking reads but never records
// fails here.
func TestEveryConfiguredKnobIsVisibleAfterFusion(t *testing.T) {
	// Distinctive values, so finding them in the output cannot be a coincidence.
	const (
		rankConstant = 37.0
		cap          = 1.75
		basePrior    = 1.1
		classPrior   = 1.9
		queryClass   = "identifier_query"
	)

	cfg := ranking.Config{
		RankConstant:     rankConstant,
		CorroborationCap: cap,
		Sources: map[recall.SourceUID]ranking.SourceConfig{
			"uid-tasks": {
				SourceID:     "tasks",
				BasePrior:    basePrior,
				IntentPriors: []ranking.IntentPrior{{QueryClass: queryClass, Effective: classPrior}},
			},
			"uid-docs": {SourceID: "docs", BasePrior: basePrior},
		},
	}
	r, err := ranking.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	resolver := lineage.MapResolver{"tasks": "uid-tasks", "docs": "uid-docs"}
	cand := func(id, uid, local string, rank int, entity string) recall.Candidate {
		return recall.Candidate{
			SourceID:  id,
			SourceUID: recall.SourceUID(uid),
			Locator:   recall.Locator{SourceID: id, SourceUID: recall.SourceUID(uid), Local: local},
			LocalRank: rank,
			Metadata:  map[string]any{ranking.MetaEntityID: entity},
		}
	}

	got, err := r.Fuse(ranking.Request{
		Resolver:   resolver,
		QueryClass: queryClass,
		Candidates: []recall.Candidate{
			cand("tasks", "uid-tasks", "td-1", 1, "e-1"),
			cand("docs", "uid-docs", "spec.md#1", 2, "e-1"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) == 0 {
		t.Fatal("no results to explain")
	}

	out := explain.Render(got.Results[0].Explanation)
	t.Logf("rendered:\n%s", out)

	for _, knob := range []struct{ name, want string }{
		{"rank constant", strconv.FormatFloat(rankConstant, 'g', 6, 64)},
		{"corroboration cap", strconv.FormatFloat(cap, 'g', 6, 64)},
		{"base prior", strconv.FormatFloat(basePrior, 'g', 6, 64)},
		{"class prior", strconv.FormatFloat(classPrior, 'g', 6, 64)},
		{"query class rule", queryClass},
	} {
		if !strings.Contains(out, knob.want) {
			t.Errorf("%s (%s) does not appear in the explanation", knob.name, knob.want)
		}
	}
}

func TestRenderOmitsWhatDidNotApply(t *testing.T) {
	out := explain.Render(recall.Explanation{
		SourceID: "tasks", LocalRank: 1, Score: 0.5, RankConstant: 60,
	})
	for _, absent := range []string{"corroboration", "generation", "lineage"} {
		if strings.Contains(out, absent) {
			t.Errorf("rendering claims %q for an explanation that carries none:\n%s", absent, out)
		}
	}
	// A reranker that did not run is stated rather than omitted: silence would
	// leave a reader unable to tell "not used" from "not reported".
	if !strings.Contains(out, "not used") {
		t.Errorf("an unused reranker should be stated:\n%s", out)
	}
}

func TestRenderIsStable(t *testing.T) {
	first := explain.Render(full())
	for range 20 {
		if got := explain.Render(full()); got != first {
			t.Fatal("rendering varies between calls")
		}
	}
	if n := strings.Count(first, "\n"); n < 8 {
		t.Errorf("rendering has %d lines, expected the full block:\n%s", n, first)
	}
	_ = fmt.Sprint(first)
}
