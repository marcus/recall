package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/eval"
)

// writeRunDir writes a candidate the way a real run leaves one: a run record
// and the per-case file beside it.
func writeRunDir(t *testing.T, run eval.Run, scores []eval.CaseScore) string {
	t.Helper()
	dir := t.TempDir()
	results := make([]eval.CaseResult, 0, len(scores))
	for _, s := range scores {
		results = append(results, eval.CaseResult{CaseID: s.CaseID, Behavior: eval.BehaviorAnswer})
	}
	if err := eval.WriteRun(dir, run, results, scores); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeBaselineFile writes a baseline the way the repository holds one: the run
// record alone, with no cases.jsonl beside it, because that file carries
// excerpts and stays out of the tree.
func writeBaselineFile(t *testing.T, run eval.Run) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "smoke.json")
	body, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runAt(id string, ndcg float64) eval.Run {
	r := eval.Run{
		SchemaVersion: 1,
		RunID:         id,
		Status:        eval.StatusPass,
		Pack:          eval.PackRef{PackID: "smoke", Version: "0.1.0", ContentHash: "sha256:same"},
		Environment:   eval.Environment{OS: "darwin", Arch: "arm64"},
	}
	r.Metrics.Cases = 41
	r.Metrics.Overall.NDCG10 = eval.Mean{Value: ndcg, N: 36}
	return r
}

// The committed baseline is a file, not a directory. A compare that could only
// read run directories could not be pointed at the one baseline the repository
// holds, which was the whole reason there was nothing to compare against.
func TestCompareReadsABaselineThatIsASingleFile(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	baseline := writeBaselineFile(t, runAt("baseline", 0.7665))
	candidate := writeRunDir(t, runAt("candidate", 0.7665), []eval.CaseScore{{CaseID: "c1"}})

	code, out, stderr := h.run("eval", "compare", baseline, candidate)
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0: %s%s", code, out, stderr)
	}
	if !strings.Contains(out, "No regression") {
		t.Errorf("an unchanged run did not report a clean comparison:\n%s", out)
	}
	// The baseline carries no per-case detail. Reporting the candidate's cases
	// as newly appeared would bury the deltas that are the point.
	if strings.Contains(out, "Cases that changed") {
		t.Errorf("cases were invented out of a baseline that never listed any:\n%s", out)
	}
}

// A machine has to be the thing that notices. The metrics quoted in a commit
// message went unchecked for ten commits precisely because a person had to
// remember to look.
func TestCompareExitsNonZeroWhenAMetricMovedDown(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	baseline := writeBaselineFile(t, runAt("baseline", 0.7665))
	candidate := writeRunDir(t, runAt("candidate", 0.7615), []eval.CaseScore{{CaseID: "c1"}})

	code, out, _ := h.run("eval", "compare", baseline, candidate)
	if code != cli.ExitInvalid {
		t.Fatalf("exit = %d, want %d: a regression left the build green\n%s", code, cli.ExitInvalid, out)
	}
	if !strings.Contains(out, "Regressed") {
		t.Errorf("the regression was not stated in the output:\n%s", out)
	}
}

// --json is what a script reads, so it must reach the same verdict as the
// rendered report. It used to exit 0 whatever it found.
func TestCompareJSONReachesTheSameVerdict(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	baseline := writeBaselineFile(t, runAt("baseline", 0.7665))
	candidate := writeRunDir(t, runAt("candidate", 0.7615), []eval.CaseScore{{CaseID: "c1"}})

	code, out, _ := h.run("eval", "compare", "--json", baseline, candidate)
	if code != cli.ExitInvalid {
		t.Fatalf("exit = %d, want %d", code, cli.ExitInvalid)
	}
	var got eval.Comparison
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if got.Acceptable() {
		t.Error("the machine-readable comparison called a loss acceptable")
	}
}

// The committed baseline is only worth having if it describes the pack that is
// committed beside it. A pack edited without refreshing the baseline makes
// every later comparison meaningless, and the loud failure belongs here rather
// than in CI, where it would read as a ranking regression.
func TestCommittedBaselinesMatchTheCommittedPacks(t *testing.T) {
	for _, name := range []string{"smoke", "dev"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "eval", "baselines", name+".json"))
			if err != nil {
				t.Fatalf("the baseline docs/evaluation.md documents is missing: %v", err)
			}
			var base eval.Run
			if err := json.Unmarshal(raw, &base); err != nil {
				t.Fatalf("the baseline is not a run record: %v", err)
			}
			if base.Status != eval.StatusPass {
				t.Errorf("status = %q: a baseline that failed a gate is not evidence", base.Status)
			}

			pack, cases, judgments, err := loadPackForTest(
				filepath.Join("..", "..", "eval", "packs", name))
			if err != nil {
				t.Fatal(err)
			}
			hash, err := eval.ContentHash(pack, cases, judgments)
			if err != nil {
				t.Fatal(err)
			}
			if base.Pack.ContentHash != hash {
				t.Errorf("baseline names pack content %s, pack is %s — refresh the baseline "+
					"with `recall eval run --pack eval/packs/%s --output $d && "+
					"cp $d/run.json eval/baselines/%s.json`",
					base.Pack.ContentHash, hash, name, name)
			}
		})
	}
}

func loadPackForTest(dir string) (*eval.Pack, []eval.Case, []eval.Judgment, error) {
	pack, err := eval.LoadPack(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	cases, err := pack.LoadCases()
	if err != nil {
		return nil, nil, nil, err
	}
	judgments, err := pack.LoadJudgments()
	if err != nil {
		return nil, nil, nil, err
	}
	return pack, cases, judgments, nil
}
