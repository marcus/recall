package eval_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/internal/recall"
)

// engine is a scriptable stand-in for the application layer, keyed by query so
// one engine can serve a whole pack.
type engine struct {
	responses     map[string]recall.QueryResponse
	queryErr      map[string]error
	expandErr     map[string]error
	expandContent map[string]string
	expandBudget  map[string]int64
	asOfSeen      map[string]*time.Time
	scopeSeen     map[string]*recall.Scope
}

func newEngine() *engine {
	return &engine{
		responses:     map[string]recall.QueryResponse{},
		queryErr:      map[string]error{},
		expandErr:     map[string]error{},
		expandContent: map[string]string{},
		expandBudget:  map[string]int64{},
		asOfSeen:      map[string]*time.Time{},
		scopeSeen:     map[string]*recall.Scope{},
	}
}

func (e *engine) Query(_ context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	e.asOfSeen[req.Query] = req.AsOf
	e.scopeSeen[req.Query] = req.Scope
	if err := e.queryErr[req.Query]; err != nil {
		return recall.QueryResponse{Outcome: recall.OutcomeFailed, Coverage: recall.CoverageDegraded}, err
	}
	return e.responses[req.Query], nil
}

func (e *engine) Expand(_ context.Context, req recall.ExpandRequest, _ string) (recall.ExpandResponse, error) {
	e.expandBudget[req.Locator.Local] = req.Budget
	if err := e.expandErr[req.Locator.Local]; err != nil {
		return recall.ExpandResponse{}, err
	}
	content := e.expandContent[req.Locator.Local]
	if content == "" {
		content = "evidence"
	}
	return recall.ExpandResponse{Content: content, SourceRevision: "rev-1"}, nil
}

// answered builds a response with one result per lineage root.
func answered(roots ...recall.LineageRoot) recall.QueryResponse {
	resp := recall.QueryResponse{Outcome: recall.OutcomeAnswered, Coverage: recall.CoverageComplete}
	for _, root := range roots {
		loc, err := root.Locator()
		if err != nil {
			panic(err)
		}
		loc.SourceID = "docs"
		resp.Results = append(resp.Results, recall.Result{
			Primary: recall.Candidate{
				SourceUID: loc.SourceUID,
				SourceID:  "docs",
				Locator:   loc,
			},
			Members:     []recall.ClusterMember{{LineageRoot: root}},
			Explanation: recall.Explanation{LineageRoot: root, SourceUID: loc.SourceUID},
		})
	}
	return resp
}

func packFor(t *testing.T, body string) *eval.Pack {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", body)
	write("cases.jsonl", "")
	write("judgments.jsonl", "")

	pack, err := eval.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

const minimalPack = `{
  "schema_version": 1,
  "pack_id": "test",
  "version": "1",
  "cases": "cases.jsonl",
  "judgments": "judgments.jsonl"
}`

func TestRunnerRecordsWhatTheEngineReturned(t *testing.T) {
	e := newEngine()
	e.responses["find the spec"] = answered("uid-docs:spec.md#1", "uid-docs:spec.md#2")

	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "find the spec", ExpectedBehavior: eval.BehaviorAnswer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Behavior != eval.BehaviorAnswer {
		t.Errorf("behavior = %q, want answer", got[0].Behavior)
	}
	if len(got[0].Ranked) != 2 {
		t.Errorf("ranked = %v, want one position per independent record", got[0].Ranked)
	}
}

// A case with an as_of is asking a historical question, so the request must
// carry the boundary. A runner that dropped it would measure a current-state
// answer against historical judgments and call the difference a ranking bug.
func TestAsOfReachesTheEngine(t *testing.T) {
	e := newEngine()
	e.responses["what did we decide"] = answered("uid-docs:a.md")

	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	if _, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "what did we decide", AsOf: &at,
		ExpectedBehavior: eval.BehaviorAnswer,
	}}); err != nil {
		t.Fatal(err)
	}
	seen := e.asOfSeen["what did we decide"]
	if seen == nil || !seen.Equal(at) {
		t.Errorf("engine saw as_of %v, want %v", seen, at)
	}
}

func TestProjectAndEntityScopeReachTheEngine(t *testing.T) {
	e := newEngine()
	e.responses["find the decision"] = answered("uid-docs:a.md")
	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	if _, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "find the decision", ExpectedBehavior: eval.BehaviorAnswer,
		Scope: &eval.CaseScope{Project: "recall", Entities: []string{"Marcus"}},
	}}); err != nil {
		t.Fatal(err)
	}
	seen := e.scopeSeen["find the decision"]
	if seen == nil || seen.Project != "recall" || len(seen.Entities) != 1 || seen.Entities[0] != "Marcus" {
		t.Fatalf("engine saw scope %+v, want project and entity", seen)
	}
}

// Latency is how long the query took, never the age of the case's boundary.
// The runner used to time a case with the same clock it pinned to the as_of, so
// a case bounded to March reported a "latency" of four months — and those
// months sat in the sample the p95 budget gate reads, which passed only because
// the nearest-rank position happened to fall below them.
func TestAPinnedAsOfIsNotALatency(t *testing.T) {
	e := newEngine()
	e.responses["what did we decide"] = answered("uid-docs:a.md")

	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "what did we decide", AsOf: &at,
		ExpectedBehavior: eval.BehaviorAnswer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Latency <= 0 || got[0].Latency > time.Minute {
		t.Errorf("latency = %v, want the duration of one in-process query", got[0].Latency)
	}
}

// A pack exists in part to check what happens when things break, so a failed
// query is a recorded result and not a broken run.
func TestFailedQueryIsARecordedResult(t *testing.T) {
	e := newEngine()
	e.queryErr["everything is down"] = errors.New("every source failed")

	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "everything is down", ExpectedBehavior: eval.BehaviorAbstain,
	}})
	if err != nil {
		t.Fatalf("a failing case must not fail the run: %v", err)
	}
	if got[0].Error == "" {
		t.Error("the failure was not recorded")
	}
}

// A ranking that returns unusable references has not retrieved anything,
// however well ordered it is.
func TestLocatorResolutionIsMeasured(t *testing.T) {
	e := newEngine()
	e.responses["q"] = answered("uid-docs:gone.md")
	e.expandErr["gone.md"] = errors.New("locator_expired")

	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "q", ExpectedBehavior: eval.BehaviorAnswer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Expansions) != 1 {
		t.Fatalf("expansions = %v", got[0].Expansions)
	}
	if got[0].Expansions[0].Root != "" {
		t.Error("a reference that did not expand was recorded as resolved")
	}
	// Display form drops the identity, so the artifact records it separately or
	// stops resolving the moment a source is renamed.
	if got[0].Expansions[0].SourceUID == "" {
		t.Error("the expansion record lost the source identity")
	}
}

func TestRunnerRecordsSourceAndExpansionAssertionInputs(t *testing.T) {
	e := newEngine()
	response := answered("uid-docs:a.md")
	response.Suppressed = []recall.Suppression{{
		Reason:      recall.SuppressLineageSeen,
		Count:       1,
		LineageRoot: "uid-docs:hidden.md",
	}}
	e.responses["q"] = response
	e.expandContent["a.md"] = "four"

	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{{
		CaseID: "c1", Query: "q", ExpectedBehavior: eval.BehaviorAnswer,
		Assertions: &eval.Assertions{MaxExpansionBytes: 8},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].ReturnedSources) != 1 || got[0].ReturnedSources[0] != "uid-docs" {
		t.Errorf("returned sources = %v, want uid-docs", got[0].ReturnedSources)
	}
	if e.expandBudget["a.md"] != 8 {
		t.Errorf("expand budget = %d, want the case ceiling 8", e.expandBudget["a.md"])
	}
	if len(got[0].Expansions) != 1 || got[0].Expansions[0].Bytes != 4 {
		t.Errorf("expansions = %+v, want 4 content bytes", got[0].Expansions)
	}
	if len(got[0].Suppressions) != 1 ||
		got[0].Suppressions[0].Reason != recall.SuppressLineageSeen ||
		got[0].Suppressions[0].LineageRoot != "uid-docs:hidden.md" {
		t.Errorf("suppressions = %+v, want positive lineage suppression telemetry",
			got[0].Suppressions)
	}
}

// A pack measuring a live endpoint is measuring something that changes
// underneath it, and its numbers are not comparable between runs.
func TestRunRefusesUndeclaredNetworkAccess(t *testing.T) {
	r := eval.NewRunner(newEngine(), packFor(t, minimalPack), eval.RunOptions{
		Locations: []eval.SourceLocation{
			{SourceID: "local", Location: "/tmp/corpus"},
			{SourceID: "remote", Location: "https://api.example.com/v1"},
		},
	})
	_, err := r.Run(context.Background(), nil)
	if !errors.Is(err, eval.ErrUndeclaredNetwork) {
		t.Fatalf("err = %v, want ErrUndeclaredNetwork", err)
	}
	if !strings.Contains(err.Error(), "remote") {
		t.Errorf("error %q does not name the offending source", err)
	}
}

func TestLivePackMayDeclareNetworkAccess(t *testing.T) {
	pack := packFor(t, `{
  "schema_version": 1, "pack_id": "live", "version": "1",
  "cases": "cases.jsonl", "judgments": "judgments.jsonl",
  "network_access": true
}`)
	r := eval.NewRunner(newEngine(), pack, eval.RunOptions{
		Locations: []eval.SourceLocation{{SourceID: "remote", Location: "https://api.example.com"}},
	})
	if _, err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("a pack that declared network access must be allowed to use it: %v", err)
	}
}

// Every behavior a pack can expect must be reachable from a real outcome.
// A vocabulary with an unreachable member is a case nobody can ever pass.
func TestEveryBehaviorIsReachable(t *testing.T) {
	e := newEngine()
	e.responses["answer"] = recall.QueryResponse{Outcome: recall.OutcomeAnswered}
	e.responses["abstain"] = recall.QueryResponse{Outcome: recall.OutcomeAbstained}
	e.responses["fail"] = recall.QueryResponse{Outcome: recall.OutcomeFailed}

	r := eval.NewRunner(e, packFor(t, minimalPack), eval.RunOptions{})
	got, err := r.Run(context.Background(), []eval.Case{
		{CaseID: "a", Query: "answer"},
		{CaseID: "b", Query: "abstain"},
		{CaseID: "c", Query: "fail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []eval.Behavior{eval.BehaviorAnswer, eval.BehaviorAbstain, eval.BehaviorFail}
	for i, w := range want {
		if got[i].Behavior != w {
			t.Errorf("case %d behavior = %q, want %q", i, got[i].Behavior, w)
		}
		if !w.Valid() {
			t.Errorf("%q is not a valid behavior, so no case could expect it", w)
		}
	}
}

func score(id string, tags []string, opts ...func(*eval.CaseScore)) eval.CaseScore {
	s := eval.CaseScore{CaseID: id, Tags: tags}
	for _, o := range opts {
		o(&s)
	}
	return s
}

// A gate is not a metric. Metrics order experiments; gates decide whether a run
// is admissible evidence at all, so one leak invalidates a run however good its
// scores are.
func TestSensitivityLeakInvalidatesTheRun(t *testing.T) {
	pack := packFor(t, minimalPack)
	scores := []eval.CaseScore{
		score("clean", []string{"docs"}),
		score("leak", []string{"docs"}, func(s *eval.CaseScore) { s.SensitivityViolations = 1 }),
	}

	gates := eval.EvaluateGates(pack, scores, eval.ReportOf(scores), nil)
	if eval.Valid(gates) {
		t.Fatal("a run that returned a record above the ceiling was accepted")
	}
	if eval.StatusOf(gates) != eval.StatusInvalid {
		t.Errorf("status = %s, want invalid", eval.StatusOf(gates))
	}
}

// One superseded record presented as current is a defect. Averaging it across a
// pack would let a large pack dilute it below notice, so the gate counts.
func TestOneForbiddenResultFailsRegardlessOfPackSize(t *testing.T) {
	pack := packFor(t, minimalPack)
	scores := []eval.CaseScore{
		score("bad", nil, func(s *eval.CaseScore) { s.Forbidden5 = eval.Value{V: 1, OK: true} }),
	}
	for i := range 200 {
		scores = append(scores, score(string(rune('a'+i%26))+string(rune('0'+i/26)), nil,
			func(s *eval.CaseScore) { s.Forbidden5 = eval.Value{V: 0, OK: true} }))
	}

	gates := eval.EvaluateGates(pack, scores, eval.ReportOf(scores), nil)
	if eval.Valid(gates) {
		t.Error("200 clean cases diluted one forbidden result into a pass")
	}
}

// A bar nobody set is not a bar something can fail. Inventing one would let the
// first run define its own passing grade.
func TestUnsetThresholdSkipsRatherThanPasses(t *testing.T) {
	pack := packFor(t, minimalPack)
	scores := []eval.CaseScore{
		score("x", []string{eval.ExactTag}, func(s *eval.CaseScore) {
			s.MRR10 = eval.Value{V: 0.2, OK: true}
		}),
	}

	gates := eval.EvaluateGates(pack, scores, eval.ReportOf(scores), nil)
	for _, g := range gates {
		if g.Name != eval.GateExactIdentifier {
			continue
		}
		if g.Status != eval.GateSkipped {
			t.Errorf("status = %s, want skipped when the pack states no threshold", g.Status)
		}
		if g.Observed == nil {
			t.Error("a skipped gate must still record the number that will become the bar")
		}
	}
}

func TestStatedThresholdIsEnforced(t *testing.T) {
	pack := packFor(t, `{
  "schema_version": 1, "pack_id": "test", "version": "1",
  "cases": "cases.jsonl", "judgments": "judgments.jsonl",
  "thresholds": {"exact_identifier_success_at_1": 0.9}
}`)
	scores := []eval.CaseScore{
		score("x", []string{eval.ExactTag}, func(s *eval.CaseScore) {
			s.MRR10 = eval.Value{V: 0.5, OK: true} // not first place
		}),
	}

	gates := eval.EvaluateGates(pack, scores, eval.ReportOf(scores), nil)
	if eval.Valid(gates) {
		t.Error("an identifier query that did not put its record first was accepted")
	}
}

func TestDeclaredZeroThresholdIsStillAThreshold(t *testing.T) {
	pack := packFor(t, `{
  "schema_version": 1, "pack_id": "test", "version": "1",
  "cases": "cases.jsonl", "judgments": "judgments.jsonl",
  "thresholds": {"exact_identifier_success_at_1": 0}
}`)
	scores := []eval.CaseScore{
		score("x", []string{eval.ExactTag}, func(s *eval.CaseScore) {
			s.MRR10 = eval.Value{V: 1, OK: true}
		}),
	}

	for _, gate := range eval.EvaluateGates(pack, scores, eval.ReportOf(scores), nil) {
		if gate.Name == eval.GateExactIdentifier && gate.Status == eval.GateSkipped {
			t.Error("a declared zero threshold was treated as an absent threshold")
		}
	}
}

func TestEveryDeclaredCaseAssertionCanFailTheRun(t *testing.T) {
	tests := []struct {
		name       string
		assertions eval.Assertions
		result     eval.CaseResult
		wantField  string
	}{
		{
			name:       "required source missing",
			assertions: eval.Assertions{RequiredSources: []recall.SourceUID{"uid-required"}},
			result:     eval.CaseResult{},
			wantField:  "required_sources",
		},
		{
			name:       "forbidden source returned",
			assertions: eval.Assertions{ForbiddenSources: []recall.SourceUID{"uid-forbidden"}},
			result:     eval.CaseResult{ReturnedSources: []recall.SourceUID{"uid-forbidden"}},
			wantField:  "forbidden_sources",
		},
		{
			name:       "latency exceeded",
			assertions: eval.Assertions{MaxLatencyMS: 5},
			result:     eval.CaseResult{Latency: 6 * time.Millisecond},
			wantField:  "max_latency_ms",
		},
		{
			name:       "expansion exceeded",
			assertions: eval.Assertions{MaxExpansionBytes: 8},
			result: eval.CaseResult{Expansions: []eval.Expansion{{
				Locator: recall.Locator{Local: "large"}, Bytes: 9,
			}}},
			wantField: "max_expansion_bytes",
		},
		{
			name:       "suppressed lineage returned",
			assertions: eval.Assertions{SuppressedLineages: []recall.LineageRoot{"uid:hidden"}},
			result:     eval.CaseResult{Ranked: []recall.LineageRoot{"uid:hidden"}},
			wantField:  "suppressed_lineages",
		},
		{
			name:       "visible lineage missing",
			assertions: eval.Assertions{VisibleLineages: []recall.LineageRoot{"uid:visible"}},
			result:     eval.CaseResult{},
			wantField:  "visible_lineages",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCase("case")
			c.Assertions = &tc.assertions
			score := eval.Score(c, nil, tc.result)
			if len(score.AssertionViolations) != 1 ||
				!strings.Contains(score.AssertionViolations[0], tc.wantField) {
				t.Fatalf("violations = %v, want one %s failure",
					score.AssertionViolations, tc.wantField)
			}
			gates := eval.EvaluateGates(
				goodPack(), []eval.CaseScore{score},
				eval.ReportOf([]eval.CaseScore{score}), nil)
			if eval.Valid(gates) {
				t.Fatal("a declared assertion failed but the run remained valid")
			}
		})
	}
}

func TestSuppressedLineageRequiresPositiveSuppressionTelemetry(t *testing.T) {
	root := recall.LineageRoot("uid:hidden")
	tests := []struct {
		name        string
		result      eval.CaseResult
		wantFailure bool
	}{
		{
			name:        "missing lineage was not suppression",
			result:      eval.CaseResult{},
			wantFailure: true,
		},
		{
			name: "different suppression reason is not proof",
			result: eval.CaseResult{Suppressions: []recall.Suppression{{
				Reason: recall.SuppressSensitivity, Count: 1, LineageRoot: root,
			}}},
			wantFailure: true,
		},
		{
			name: "zero suppressed candidates is not proof",
			result: eval.CaseResult{Suppressions: []recall.Suppression{{
				Reason: recall.SuppressLineageSeen, Count: 0, LineageRoot: root,
			}}},
			wantFailure: true,
		},
		{
			name: "retrieved candidate suppressed for prior display",
			result: eval.CaseResult{Suppressions: []recall.Suppression{{
				Reason: recall.SuppressLineageSeen, Count: 1, LineageRoot: root,
			}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCase("case")
			c.Assertions = &eval.Assertions{SuppressedLineages: []recall.LineageRoot{root}}
			score := eval.Score(c, nil, tc.result)
			failed := len(score.AssertionViolations) > 0
			if failed != tc.wantFailure {
				t.Fatalf("violations = %v, want failure %v",
					score.AssertionViolations, tc.wantFailure)
			}
		})
	}
}

func TestPositiveExpansionAssertionsDoNotPassWithoutResults(t *testing.T) {
	root := recall.LineageRoot("uid:expected")
	tests := []struct {
		name       string
		assertions eval.Assertions
		wantField  string
	}{
		{
			name:       "byte ceiling needs an expansion",
			assertions: eval.Assertions{MaxExpansionBytes: 512},
			wantField:  "max_expansion_bytes",
		},
		{
			name: "expected revision needs its lineage expansion",
			assertions: eval.Assertions{
				ExpectedRevisions: map[recall.LineageRoot]string{root: "rev-1"},
			},
			wantField: "expected_revisions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCase("case")
			c.ExpectedBehavior = eval.BehaviorAnswer
			c.Assertions = &tc.assertions
			score := eval.Score(c, nil, eval.CaseResult{})
			if len(score.AssertionViolations) != 1 ||
				!strings.Contains(score.AssertionViolations[0], tc.wantField) {
				t.Fatalf("violations = %v, want one %s precondition failure",
					score.AssertionViolations, tc.wantField)
			}
		})
	}
}

func TestExpectedAbstentionExplicitlyPermitsNoExpansion(t *testing.T) {
	c := goodCase("case")
	c.ExpectedBehavior = eval.BehaviorAbstain
	c.Assertions = &eval.Assertions{MaxExpansionBytes: 512}

	score := eval.Score(c, nil, eval.CaseResult{Behavior: eval.BehaviorAbstain})
	if len(score.AssertionViolations) != 0 {
		t.Fatalf("an expected no-result case failed an expansion ceiling: %v",
			score.AssertionViolations)
	}
}

// Comparing runs over different pack content produces a number with no meaning
// that somebody would nonetheless act on.
func TestComparisonRefusesRunsThatMeasuredDifferentThings(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass, Pack: eval.PackRef{ContentHash: "hash-one"}}
	cand := eval.Run{RunID: "b", Status: eval.StatusPass, Pack: eval.PackRef{ContentHash: "hash-two"}}

	got := eval.Compare(base, cand, nil, nil)
	if got.Comparable {
		t.Error("runs over different pack content were reported as comparable")
	}
	if len(got.Blockers) == 0 {
		t.Error("no reason was given")
	}
}

func TestComparisonRefusesAnInvalidRun(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass}
	cand := eval.Run{RunID: "b", Status: eval.StatusInvalid}

	if eval.Compare(base, cand, nil, nil).Comparable {
		t.Error("a run that failed a gate was compared as if it were evidence")
	}
}

// A metric that went from undefined to defined has not improved; it has started
// being measured.
func TestUndefinedMetricsAreNotAChange(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass}
	cand := eval.Run{RunID: "b", Status: eval.StatusPass}
	cand.Metrics.Overall.NDCG10 = eval.Mean{Value: 0.8, N: 4}

	got := eval.Compare(base, cand, nil, nil)
	for _, d := range got.Overall {
		if d.Metric == "ndcg_at_10" {
			if d.Defined {
				t.Error("a metric with no baseline was reported as a change")
			}
			if d.Change != 0 {
				t.Errorf("change = %v, want none", d.Change)
			}
		}
	}
}

// The whole argument for a committed baseline is that a machine says no. A
// comparison that reported a loss and still called itself acceptable would let
// drift through a green build exactly as an absent baseline did.
func TestARateThatMovedDownIsNotAcceptable(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass}
	base.Metrics.Overall.NDCG10 = eval.Mean{Value: 0.7665, N: 36}
	cand := eval.Run{RunID: "b", Status: eval.StatusPass}
	cand.Metrics.Overall.NDCG10 = eval.Mean{Value: 0.7615, N: 36}

	got := eval.Compare(base, cand, nil, nil)
	if got.Acceptable() {
		t.Error("nDCG@10 lost five thousandths and the comparison was acceptable")
	}
	if len(got.Regressions) != 1 {
		t.Fatalf("regressions = %v, want the one metric that moved", got.Regressions)
	}
	if !strings.Contains(eval.SummarizeComparison(got), "Regressed") {
		t.Error("the rendered comparison did not lead with the verdict")
	}
}

// A gate that fires on the last bit of a float is a gate people route around.
// The smallest move one case can make to a forty-case average is around 1e-3,
// so noise this far below it is a rounding difference between two machines and
// not a quality regression.
func TestFloatNoiseIsNotARegression(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass}
	base.Metrics.Overall.NDCG10 = eval.Mean{Value: 0.7664585878796282, N: 36}
	cand := eval.Run{RunID: "b", Status: eval.StatusPass}
	cand.Metrics.Overall.NDCG10 = eval.Mean{Value: 0.7664585878796281, N: 36}

	got := eval.Compare(base, cand, nil, nil)
	if !got.Acceptable() {
		t.Errorf("a one-bit difference was called a regression: %v", got.Regressions)
	}
}

// The committed baseline is a run record with no cases.jsonl beside it, because
// that file carries excerpts and stays out of the repository. Reading the
// absence as a set of newly appeared cases would bury the metric deltas under a
// page of noise about nothing having happened.
func TestABaselineWithoutPerCaseDetailInventsNoCaseChanges(t *testing.T) {
	base := eval.Run{RunID: "a", Status: eval.StatusPass}
	cand := eval.Run{RunID: "b", Status: eval.StatusPass}

	got := eval.Compare(base, cand, nil, []eval.CaseScore{{CaseID: "c1"}, {CaseID: "c2"}})
	if len(got.Cases) != 0 {
		t.Errorf("cases = %v, want none: the baseline never claimed they were absent", got.Cases)
	}
	if !got.Acceptable() {
		t.Errorf("a baseline without per-case detail was treated as a loss: %v", got.Regressions)
	}
}
