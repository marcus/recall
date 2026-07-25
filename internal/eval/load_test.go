package eval_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/eval"
)

const manifestJSON = `{
  "schema_version": 1,
  "pack_id": "smoke",
  "version": "0.1.0",
  "description": "fixture",
  "profile": "smoke",
  "cases": "cases.jsonl",
  "judgments": "judgments.jsonl",
  "budgets": {"p95_latency_ms": 250},
  "thresholds": {"abstention_accuracy": 0.9},
  "sensitivity_ceiling": "internal"
}`

const casesJSONL = `{"schema_version":1,"case_id":"tasks-exact-001","query":"What is the state of td-f62256?","profile":"smoke","as_of":"2026-07-23T12:00:00-07:00","expected_behavior":"answer","tags":["exact","task"],"timeout_ms":250,"notes":"Exact identifier must reach the top."}

{"schema_version":1,"case_id":"tasks-abstain-002","query":"who owns the moon","profile":"smoke","expected_behavior":"abstain","tags":["no-answer"],"assertions":{"expected_coverage":"complete"}}
`

const judgmentsJSONL = `{"schema_version":1,"case_id":"tasks-exact-001","lineage_root":"01J8ZK:td-f62256","relevance":3,"required":true,"supports":["state","title"]}
{"schema_version":1,"case_id":"tasks-exact-001","lineage_root":"01J8ZK:td-f00000","relevance":0,"forbidden":true}
`

// writePack lays out a pack on disk. Contents are overridable so a test can
// break exactly one thing.
func writePack(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	defaults := map[string]string{
		eval.PackFile:     manifestJSON,
		"cases.jsonl":     casesJSONL,
		"judgments.jsonl": judgmentsJSONL,
	}
	for name, body := range defaults {
		if override, ok := files[name]; ok {
			body = override
		}
		if body == "" {
			continue // an empty override means "do not write this file"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if _, standard := defaults[name]; standard {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadPackReadsTheManifest(t *testing.T) {
	p, err := eval.LoadPack(writePack(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.PackID != "smoke" || p.Version != "0.1.0" {
		t.Errorf("pack = %+v", p)
	}
	if p.Budgets == nil || p.Budgets.P95LatencyMS != 250 {
		t.Errorf("budgets = %+v", p.Budgets)
	}
	if p.SensitivityCeiling == nil || p.SensitivityCeiling.String() != "internal" {
		t.Errorf("sensitivity ceiling = %v", p.SensitivityCeiling)
	}
	// An undeclared ceiling and a public ceiling are different statements, so
	// the field has to be able to be absent.
	if p.NetworkAccess {
		t.Error("network access defaulted to true")
	}
}

// The separation the whole layout exists for: a runner obtains the queries
// without ever opening the answers. Deleting the judgment file proves it,
// because no amount of reading the code proves a call did not happen.
func TestLoadCasesNeverOpensTheJudgmentFile(t *testing.T) {
	dir := writePack(t, nil)
	if err := os.Remove(filepath.Join(dir, "judgments.jsonl")); err != nil {
		t.Fatal(err)
	}

	p, err := eval.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := p.LoadCases()
	if err != nil {
		t.Fatalf("loading cases needed the judgments: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("loaded %d cases, want 2", len(cases))
	}
	if _, err := p.LoadJudgments(); err == nil {
		t.Error("judgments loaded from a file that is not there")
	}
}

func TestLoadCasesParsesEveryField(t *testing.T) {
	p, err := eval.LoadPack(writePack(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := p.LoadCases()
	if err != nil {
		t.Fatal(err)
	}

	first := cases[0]
	if first.CaseID != "tasks-exact-001" || first.ExpectedBehavior != eval.BehaviorAnswer {
		t.Errorf("case = %+v", first)
	}
	if first.AsOf == nil || first.AsOf.UTC().Format("2006-01-02T15:04:05Z") != "2026-07-23T19:00:00Z" {
		t.Errorf("as_of = %v", first.AsOf)
	}
	if len(first.Tags) != 2 {
		t.Errorf("tags = %v", first.Tags)
	}
	second := cases[1]
	if second.Assertions == nil || second.Assertions.ExpectedCoverage != "complete" {
		t.Errorf("assertions = %+v", second.Assertions)
	}
}

func TestLoadJudgmentsParsesGradesAndFlags(t *testing.T) {
	p, err := eval.LoadPack(writePack(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	judgments, err := p.LoadJudgments()
	if err != nil {
		t.Fatal(err)
	}
	if len(judgments) != 2 {
		t.Fatalf("loaded %d judgments, want 2", len(judgments))
	}
	if judgments[0].Relevance != eval.Authoritative || !judgments[0].Required {
		t.Errorf("judgment = %+v", judgments[0])
	}
	if !judgments[1].Forbidden || judgments[1].Relevance != eval.Irrelevant {
		t.Errorf("judgment = %+v", judgments[1])
	}
}

// A pack is edited by hand. "Invalid document" with no line number is not
// something anyone can act on.
func TestLoadErrorNamesTheFileAndLine(t *testing.T) {
	broken := judgmentsJSONL + `{"schema_version":1,"case_id":"tasks-exact-001","lineage_root":"01J8ZK:x","relevance":9}` + "\n"
	_, err := mustPack(t, writePack(t, map[string]string{"judgments.jsonl": broken})).LoadJudgments()
	if err == nil {
		t.Fatal("accepted relevance 9")
	}
	if !strings.Contains(err.Error(), "judgments.jsonl:3") {
		t.Errorf("error does not name the line: %v", err)
	}
}

// A pack is a portable artifact. A path leaving its directory would make the
// pack depend on where it happens to be unpacked, and is a way to read files
// the pack has no business reading.
func TestLoadPackRefusesAPathOutsideThePack(t *testing.T) {
	manifest := strings.Replace(manifestJSON, `"cases.jsonl"`, `"../elsewhere/cases.jsonl"`, 1)
	_, err := eval.LoadPack(writePack(t, map[string]string{eval.PackFile: manifest}))
	if !errors.Is(err, eval.ErrUnsafePackPath) {
		t.Fatalf("err = %v, want ErrUnsafePackPath", err)
	}
}

// The schema forbids unknown properties and the Go types are checked against
// the same document. A field in one and not the other is drift, and drift in a
// pack format silently drops judgments.
func TestLoadRejectsAnUnknownField(t *testing.T) {
	extra := `{"schema_version":1,"case_id":"c","query":"q","profile":"smoke","expected_behavior":"answer","weight":2}` + "\n"
	_, err := mustPack(t, writePack(t, map[string]string{"cases.jsonl": extra})).LoadCases()
	if err == nil {
		t.Fatal("accepted an unknown field")
	}
}

// Blank lines are how a hand-edited JSONL file separates groups of records.
// They are not records.
func TestBlankLinesAreNotRecords(t *testing.T) {
	cases, err := mustPack(t, writePack(t, nil)).LoadCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Errorf("loaded %d cases from a file with a blank line, want 2", len(cases))
	}
}

func mustPack(t *testing.T, dir string) *eval.Pack {
	t.Helper()
	p, err := eval.LoadPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
