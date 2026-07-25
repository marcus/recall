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
	responses map[string]recall.QueryResponse
	queryErr  map[string]error
	expandErr map[string]error
	asOfSeen  map[string]*time.Time
}

func newEngine() *engine {
	return &engine{
		responses: map[string]recall.QueryResponse{},
		queryErr:  map[string]error{},
		expandErr: map[string]error{},
		asOfSeen:  map[string]*time.Time{},
	}
}

func (e *engine) Query(_ context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	e.asOfSeen[req.Query] = req.AsOf
	if err := e.queryErr[req.Query]; err != nil {
		return recall.QueryResponse{Outcome: recall.OutcomeFailed, Coverage: recall.CoverageDegraded}, err
	}
	return e.responses[req.Query], nil
}

func (e *engine) Expand(_ context.Context, req recall.ExpandRequest, _ string) (recall.ExpandResponse, error) {
	if err := e.expandErr[req.Locator.Local]; err != nil {
		return recall.ExpandResponse{}, err
	}
	return recall.ExpandResponse{Content: "evidence", SourceRevision: "rev-1"}, nil
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
