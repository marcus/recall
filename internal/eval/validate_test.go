package eval_test

import (
	"errors"
	"testing"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/recall"
)

func goodPack() *eval.Pack {
	return &eval.Pack{
		SchemaVersion: eval.SchemaVersion,
		PackID:        "smoke",
		Version:       "0.1.0",
		Cases:         "cases.jsonl",
		Judgments:     "judgments.jsonl",
	}
}

func goodCase(id string) eval.Case {
	return eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           id,
		Query:            "q",
		Profile:          "smoke",
		ExpectedBehavior: eval.BehaviorAnswer,
	}
}

func goodJudgment(caseID string, r recall.LineageRoot) eval.Judgment {
	return eval.Judgment{
		SchemaVersion: eval.SchemaVersion,
		CaseID:        caseID,
		LineageRoot:   r,
		Relevance:     eval.Authoritative,
		Required:      true,
	}
}

func TestValidateAcceptsAConsistentPack(t *testing.T) {
	err := eval.Validate(goodPack(),
		[]eval.Case{goodCase("a"), goodCase("b")},
		[]eval.Judgment{goodJudgment("a", "uid:1"), goodJudgment("b", "uid:2")})
	if err != nil {
		t.Fatal(err)
	}
}

// Each of these is a mistake a single-document schema cannot see, because each
// is a statement about the pack as a whole.
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name      string
		why       string
		pack      func(*eval.Pack)
		cases     []eval.Case
		judgments []eval.Judgment
		want      error
	}{
		{
			name:  "judgment for a case that does not exist",
			why:   "the judgment never fires, so a metric silently loses its ground truth",
			cases: []eval.Case{goodCase("a")},
			judgments: []eval.Judgment{
				goodJudgment("a", "uid:1"),
				goodJudgment("typo-in-case-id", "uid:2"),
			},
			want: eval.ErrUnknownCase,
		},
		{
			name:  "relevance grade outside the vocabulary",
			why:   "graded metrics weight by the numeric value, so an unknown grade changes a score instead of failing",
			cases: []eval.Case{goodCase("a")},
			judgments: []eval.Judgment{
				{SchemaVersion: 1, CaseID: "a", LineageRoot: "uid:1", Relevance: 7},
			},
			want: eval.ErrUnknownGrade,
		},
		{
			name:  "lineage root that is not a persisted locator",
			why:   "a root that cannot be parsed can never match a candidate's, so the judgment passes vacuously forever",
			cases: []eval.Case{goodCase("a")},
			judgments: []eval.Judgment{
				{SchemaVersion: 1, CaseID: "a", LineageRoot: "td-f62256", Relevance: 3},
			},
			want: eval.ErrMalformedLineageRoot,
		},
		{
			name: "expected_fail naming an assertion the case does not declare",
			why:  "nothing can violate an assertion nobody stated, so the marking reports itself fixed forever",
			cases: []eval.Case{func() eval.Case {
				c := goodCase("a")
				c.ExpectedFail = &eval.ExpectedFail{
					Reason:     "td-b94f6e: excerpts are cut at index time",
					Assertions: []string{"excerpt_contains"},
				}
				c.Assertions = &eval.Assertions{RequiredSources: []recall.SourceUID{"uid"}}
				return c
			}()},
			want: eval.ErrVacuousExpectedFail,
		},
		{
			name: "expected_fail naming something outside the assertion vocabulary",
			why:  "a typo excuses nothing and reports itself as a defect fix on the first run",
			cases: []eval.Case{func() eval.Case {
				c := goodCase("a")
				c.ExpectedFail = &eval.ExpectedFail{
					Reason:     "td-b94f6e: excerpts are cut at index time",
					Assertions: []string{"excerpt_contain"},
				}
				c.Assertions = &eval.Assertions{ExcerptContains: map[recall.LineageRoot][]string{
					"uid:doc": {"blog"},
				}}
				return c
			}()},
			want: eval.ErrUnknownAssertionName,
		},
		{
			name: "result bounds no run can satisfy",
			why:  "the case would fail forever for a reason that is in the pack rather than in the system",
			cases: []eval.Case{func() eval.Case {
				c := goodCase("a")
				two, one := 2, 1
				c.Assertions = &eval.Assertions{MinResults: &two, MaxResults: &one}
				return c
			}()},
			want: eval.ErrImpossibleResultBounds,
		},
		{
			name: "expected top lineage that is not a persisted locator",
			why:  "a root that cannot be parsed never matches a result, so the assertion passes vacuously",
			cases: []eval.Case{func() eval.Case {
				c := goodCase("a")
				c.Assertions = &eval.Assertions{ExpectedTopLineage: "d7c7a8a8"}
				return c
			}()},
			want: eval.ErrMalformedLineageRoot,
		},
		{
			name: "excerpt assertion keyed on a malformed root",
			why:  "the same vacuous pass, on the one assertion that reads what a result said",
			cases: []eval.Case{func() eval.Case {
				c := goodCase("a")
				c.Assertions = &eval.Assertions{ExcerptContains: map[recall.LineageRoot][]string{
					"profile/TOOLS.md": {"blog"},
				}}
				return c
			}()},
			want: eval.ErrMalformedLineageRoot,
		},
		{
			name: "schema version this build does not implement",
			why:  "a reader that ignores fields it does not know would report metrics over content it only partly understood",
			pack: func(p *eval.Pack) { p.SchemaVersion = eval.SchemaVersion + 1 },
			want: eval.ErrUnsupportedSchema,
		},
		{
			name:  "two cases with one ID",
			why:   "judgments key on the ID, so a duplicate makes every judgment ambiguous",
			cases: []eval.Case{goodCase("a"), goodCase("a")},
			want:  eval.ErrDuplicateCase,
		},
		{
			name:  "one lineage root graded twice for one case",
			why:   "the two grades disagree about the same evidence and nothing says which wins",
			cases: []eval.Case{goodCase("a")},
			judgments: []eval.Judgment{
				goodJudgment("a", "uid:1"),
				{SchemaVersion: 1, CaseID: "a", LineageRoot: "uid:1", Relevance: 1},
			},
			want: eval.ErrDuplicateJudgment,
		},
		{
			name:  "evidence that must and must not be retrieved",
			why:   "no run can satisfy it, so the case is unpassable by construction",
			cases: []eval.Case{goodCase("a")},
			judgments: []eval.Judgment{
				{SchemaVersion: 1, CaseID: "a", LineageRoot: "uid:1", Relevance: 3, Required: true, Forbidden: true},
			},
			want: eval.ErrContradictoryJudgment,
		},
		{
			name:  "expected behavior outside answer, clarify, abstain",
			why:   "abstention accuracy would compare against a behavior nothing can produce",
			cases: []eval.Case{{SchemaVersion: 1, CaseID: "a", Query: "q", Profile: "p", ExpectedBehavior: "guess"}},
			want:  eval.ErrInvalidBehavior,
		},
		{
			name: "assertion keyed on a malformed lineage root",
			why:  "the assertion can never match a candidate, so it passes without ever being checked",
			cases: []eval.Case{{
				SchemaVersion: 1, CaseID: "a", Query: "q", Profile: "p",
				ExpectedBehavior: eval.BehaviorAnswer,
				Assertions:       &eval.Assertions{SuppressedLineages: []recall.LineageRoot{"bare-local"}},
			}},
			want: eval.ErrMalformedLineageRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := goodPack()
			if tc.pack != nil {
				tc.pack(p)
			}
			err := eval.Validate(p, tc.cases, tc.judgments)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v\nwhy this matters: %s", err, tc.want, tc.why)
			}
		})
	}
}

// A pack is edited by hand, and one round trip per mistake is a bad way to
// spend a morning.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	err := eval.Validate(goodPack(),
		[]eval.Case{goodCase("a")},
		[]eval.Judgment{
			{SchemaVersion: 1, CaseID: "ghost", LineageRoot: "bare", Relevance: 9},
		})
	if err == nil {
		t.Fatal("accepted three problems at once")
	}
	for _, want := range []error{eval.ErrUnknownCase, eval.ErrMalformedLineageRoot, eval.ErrUnknownGrade} {
		if !errors.Is(err, want) {
			t.Errorf("%v was not reported: %v", want, err)
		}
	}
}

func TestValidateRejectsUnsupportedThresholds(t *testing.T) {
	pack := goodPack()
	pack.Thresholds = map[string]float64{"ndcg_at_10": 0.9}

	err := eval.Validate(pack, []eval.Case{goodCase("a")},
		[]eval.Judgment{goodJudgment("a", "uid:1")})
	if !errors.Is(err, eval.ErrUnsupportedThreshold) {
		t.Fatalf("err = %v, want ErrUnsupportedThreshold", err)
	}
}
