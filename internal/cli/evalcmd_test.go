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

func sourceFamilyRegressionRun(id string, familyNDCG float64) eval.Run {
	r := runAt(id, 0.8)
	stable := eval.Rates{NDCG10: eval.Mean{Value: 0.8, N: 1}}
	family := eval.Rates{NDCG10: eval.Mean{Value: familyNDCG, N: 1}}
	r.Metrics.ByTag = eval.GroupReport{
		Groups: map[string]eval.Metrics{
			"shared": {Rates: stable},
		},
		Macro: eval.Macro{Rates: stable, Groups: 1},
	}
	r.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{
			"shared": {Rates: family},
		},
		Macro: eval.Macro{Rates: family, Groups: 1},
	}
	return r
}

// This pair keeps overall and a same-named tag stable while only the source
// family loses quality. It is the regression the old comparison omitted:
// rendering looked clean and CI exited zero because BySourceFamily never
// reached the verdict.
func TestCompareRejectsHiddenSourceFamilyLoss(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	baselineRun := sourceFamilyRegressionRun("baseline", 0.8)
	currentRun := sourceFamilyRegressionRun("current", 0.7)
	baseline := writeBaselineFile(t, baselineRun)
	current := writeRunDir(t, currentRun, []eval.CaseScore{{CaseID: "c1"}})

	code, out, stderr := h.run("eval", "compare", baseline, current)
	if code != cli.ExitInvalid {
		t.Fatalf("exit = %d, want %d: source-family loss left CI green\n%s%s",
			code, cli.ExitInvalid, out, stderr)
	}
	for _, want := range []string{
		"Regressed",
		`source family group "shared"/ndcg_at_10`,
		"source family macro/ndcg_at_10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human comparison does not identify %q:\n%s", want, out)
		}
	}

	code, out, stderr = h.run("eval", "compare", "--json", baseline, current)
	if code != cli.ExitInvalid {
		t.Fatalf("JSON exit = %d, want %d: %s%s", code, cli.ExitInvalid, out, stderr)
	}
	var got eval.Comparison
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if got.Acceptable() {
		t.Fatal("JSON comparison called the source-family loss acceptable")
	}
	if len(got.Regressions) != 2 {
		t.Fatalf("JSON regressions = %+v, want family group and macro", got.Regressions)
	}
	for _, regression := range got.Regressions {
		if regression.Dimension != "source_family" ||
			!strings.HasPrefix(regression.Key, "source_family:") {
			t.Errorf("JSON regression is dimension-ambiguous: %+v", regression)
		}
	}
}

// This is the more dangerous source-family loss: the source stops
// contributing, so its metric becomes undefined instead of numerically lower.
// Overall and the same-named tag stay stable, and both the family group and
// family macro must still make the command say no.
func TestCompareRejectsMissingSourceFamilyInHumanAndJSON(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})

	baselineRun := sourceFamilyRegressionRun("baseline", 0.8)
	currentRun := sourceFamilyRegressionRun("current", 0.8)
	currentRun.Metrics.BySourceFamily = eval.GroupReport{
		Groups: map[string]eval.Metrics{},
	}
	baseline := writeBaselineFile(t, baselineRun)
	current := writeRunDir(t, currentRun, []eval.CaseScore{{CaseID: "c1"}})

	code, out, stderr := h.run("eval", "compare", baseline, current)
	if code != cli.ExitInvalid {
		t.Fatalf("exit = %d, want %d: missing family left CI green\n%s%s",
			code, cli.ExitInvalid, out, stderr)
	}
	for _, want := range []string{
		`source family group "shared"/ndcg_at_10`,
		"source family macro/ndcg_at_10",
		"0.8000 (n=1) → n/a (n=0); population disappeared",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human comparison does not identify %q:\n%s", want, out)
		}
	}

	code, out, stderr = h.run("eval", "compare", "--json", baseline, current)
	if code != cli.ExitInvalid {
		t.Fatalf("JSON exit = %d, want %d: %s%s", code, cli.ExitInvalid, out, stderr)
	}
	var got eval.Comparison
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if got.Acceptable() || len(got.Regressions) != 2 {
		t.Fatalf("JSON verdict/regressions = %v / %+v",
			got.Acceptable(), got.Regressions)
	}
	for _, regression := range got.Regressions {
		if regression.BaselineN != 1 ||
			regression.CandidateN != 0 ||
			regression.Defined {
			t.Errorf("JSON hides population loss: %+v", regression)
		}
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
	for _, name := range []string{"smoke", "firstuse"} {
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

func TestEvalValidateUsesOnlyAnExplicitOrUserConfiguredPack(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	code, _, stderr := h.run("eval", "validate", "--json")
	if code != cli.ExitError {
		t.Fatalf("without configured path exit = %d, want %d", code, cli.ExitError)
	}
	if !strings.Contains(stderr, "evaluation.development_pack") {
		t.Fatalf("missing-path error does not name user configuration:\n%s", stderr)
	}

	pack, err := filepath.Abs(filepath.Join("..", "..", "eval", "packs", "smoke"))
	if err != nil {
		t.Fatal(err)
	}
	h = newHarness(t, harnessOptions{userTOML: `
[evaluation]
development_pack = "` + pack + `"
`})
	code, out, stderr := h.run("eval", "validate", "--json")
	if code != cli.ExitOK {
		t.Fatalf("configured pack exit = %d: %s%s", code, out, stderr)
	}
	var got struct {
		PackID string `json:"pack_id"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.PackID != "smoke" {
		t.Fatalf("pack_id = %q, want smoke", got.PackID)
	}
}

func TestEvalRunHasNoBuiltInDevelopmentPack(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	code, _, stderr := h.run("eval", "run", "--json")
	if code != cli.ExitError {
		t.Fatalf("without configured path exit = %d, want %d", code, cli.ExitError)
	}
	if !strings.Contains(stderr, "evaluation.development_pack") {
		t.Fatalf("missing-path error does not name user configuration:\n%s", stderr)
	}
}

func TestEvalCompareUsesOnlyAnExplicitOrUserConfiguredBaseline(t *testing.T) {
	candidate := writeRunDir(t, runAt("candidate", 0.7665), []eval.CaseScore{{CaseID: "c1"}})

	h := newHarness(t, harnessOptions{})
	code, _, stderr := h.run("eval", "compare", candidate)
	if code != cli.ExitError {
		t.Fatalf("without configured baseline exit = %d, want %d", code, cli.ExitError)
	}
	if !strings.Contains(stderr, "evaluation.development_baseline") {
		t.Fatalf("missing-baseline error does not name user configuration:\n%s", stderr)
	}

	baseline := writeBaselineFile(t, runAt("baseline", 0.7665))
	h = newHarness(t, harnessOptions{userTOML: `
[evaluation]
development_baseline = "` + baseline + `"
`})
	code, out, stderr := h.run("eval", "compare", candidate)
	if code != cli.ExitOK {
		t.Fatalf("configured baseline exit = %d: %s%s", code, out, stderr)
	}
	if !strings.Contains(out, "No regression") {
		t.Fatalf("configured comparison did not run:\n%s", out)
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
