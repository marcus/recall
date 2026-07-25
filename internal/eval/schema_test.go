package eval_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/internal/recall"
)

func TestEverySchemaCompiles(t *testing.T) {
	for _, kind := range eval.Kinds {
		if err := eval.ValidateBytes(kind, []byte(`{}`)); err == nil {
			t.Errorf("%s schema accepted an empty object", kind)
		}
	}
}

// A schema file nothing can reach is a schema nothing enforces.
func TestEverySchemaHasAKind(t *testing.T) {
	files, err := eval.SchemaFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(eval.Kinds) {
		t.Errorf("eval/schema holds %d schemas (%v) but only %d kinds reach them",
			len(files), files, len(eval.Kinds))
	}
}

// The Go types and the schemas describe one format. Marshalling a fully
// populated value and validating it is what keeps them from drifting: a field
// renamed on one side and not the other fails here rather than in a run.
func TestPopulatedTypesValidateAgainstTheirSchema(t *testing.T) {
	cases := []struct {
		kind  eval.Kind
		value any
	}{
		{eval.KindPack, fullPack()},
		{eval.KindCase, fullCase()},
		{eval.KindJudgment, fullJudgment()},
		{eval.KindRun, fullRun()},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := eval.ValidateBytes(tc.kind, raw); err != nil {
				t.Fatalf("%v\ndocument: %s", err, raw)
			}
		})
	}
}

// The vocabulary a schema enforces on its own, before any cross-document
// check: a grade outside the scale, a locator that names no source, a property
// nobody defined.
func TestSchemaRejects(t *testing.T) {
	cases := []struct {
		name string
		kind eval.Kind
		doc  string
	}{
		{
			"relevance outside the scale",
			eval.KindJudgment,
			`{"schema_version":1,"case_id":"a","lineage_root":"uid:1","relevance":4}`,
		},
		{
			"lineage root with no source",
			eval.KindJudgment,
			`{"schema_version":1,"case_id":"a","lineage_root":"td-f62256","relevance":3}`,
		},
		{
			"lineage root with no local part",
			eval.KindJudgment,
			`{"schema_version":1,"case_id":"a","lineage_root":"uid:","relevance":3}`,
		},
		{
			"behavior outside the vocabulary",
			eval.KindCase,
			`{"schema_version":1,"case_id":"a","query":"q","profile":"p","expected_behavior":"guess"}`,
		},
		{
			"as_of that is not a timestamp",
			eval.KindCase,
			`{"schema_version":1,"case_id":"a","query":"q","profile":"p","expected_behavior":"answer","as_of":"yesterday"}`,
		},
		{
			"a property nobody defined",
			eval.KindCase,
			`{"schema_version":1,"case_id":"a","query":"q","profile":"p","expected_behavior":"answer","weight":2}`,
		},
		{
			"an absolute path in a pack",
			eval.KindPack,
			`{"schema_version":1,"pack_id":"p","version":"1","cases":"/etc/passwd","judgments":"j.jsonl"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := eval.ValidateBytes(tc.kind, []byte(tc.doc)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// A run is only worth comparing to another run if both name the pack content
// they measured, and these are the two ways that claim goes wrong: a pack
// identity that is not a hash, and a metrics block missing a metric.
//
// Each starts from a run the schema accepts and breaks one thing, so a
// rejection can only be about that thing.
func TestRunSchemaRejectsABrokenClaim(t *testing.T) {
	valid, err := json.Marshal(fullRun())
	if err != nil {
		t.Fatal(err)
	}
	if err := eval.ValidateBytes(eval.KindRun, valid); err != nil {
		t.Fatalf("the unbroken run must validate first: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"pack identity that is not a content hash", func(doc map[string]any) {
			doc["pack"].(map[string]any)["content_hash"] = "abc"
		}},
		{"a metric missing from the overall numbers", func(doc map[string]any) {
			overall := doc["metrics"].(map[string]any)["overall"].(map[string]any)
			delete(overall, "ndcg_at_10")
		}},
		{"a metric the vocabulary does not contain", func(doc map[string]any) {
			overall := doc["metrics"].(map[string]any)["overall"].(map[string]any)
			overall["vibes_at_5"] = map[string]any{"value": 1, "n": 1}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(valid, &doc); err != nil {
				t.Fatal(err)
			}
			tc.mutate(doc)
			broken, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := eval.ValidateBytes(eval.KindRun, broken); err == nil {
				t.Errorf("accepted: %s", broken)
			}
		})
	}
}

func fullPack() eval.Pack {
	ceiling := recall.SensitivityInternal
	return eval.Pack{
		SchemaVersion:      eval.SchemaVersion,
		PackID:             "smoke",
		Version:            "0.1.0",
		Description:        "synthetic, network-free, runs on every change",
		Profile:            "smoke",
		Cases:              "cases.jsonl",
		Judgments:          "judgments.jsonl",
		Sources:            "sources",
		Transcripts:        "transcripts",
		NetworkAccess:      false,
		Budgets:            &eval.Budgets{P95LatencyMS: 250, ModelCalls: 0, Tokens: 0, ExternalRequests: 0},
		Thresholds:         map[string]float64{"abstention_accuracy": 0.9, "exact_identifier_success_at_1": 0.95},
		SensitivityCeiling: &ceiling,
	}
}

func fullCase() eval.Case {
	asOf := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	ceiling := recall.SensitivityConfidential
	return eval.Case{
		SchemaVersion:    eval.SchemaVersion,
		CaseID:           "tasks-exact-001",
		Query:            "What is the state of td-f62256?",
		Profile:          "smoke",
		AsOf:             &asOf,
		ExpectedBehavior: eval.BehaviorAnswer,
		Tags:             []string{"exact", "task", "current-state"},
		TimeoutMS:        250,
		Notes:            "Exact stable identifier must reach the top of the fused list.",
		Assertions: &eval.Assertions{
			ExpectedCoverage: recall.CoverageDegraded,
			ExpectedSourceOutcomes: map[recall.SourceUID]recall.SearchOutcome{
				"01J8ZKQ4M7": recall.SearchSuccess,
				"01J8ZKQ4M8": recall.SearchTimeout,
			},
			RequiredSources:    []recall.SourceUID{"01J8ZKQ4M7"},
			ForbiddenSources:   []recall.SourceUID{"01J8ZKQ4M9"},
			MaxLatencyMS:       250,
			MaxExpansionBytes:  8192,
			ExpectedRevisions:  map[recall.LineageRoot]string{"01J8ZKQ4M7:td-f62256": "cursor-4412"},
			SuppressedLineages: []recall.LineageRoot{"01J8ZKQ4M8:sig-8000"},
			VisibleLineages:    []recall.LineageRoot{"01J8ZKQ4M7:td-f62256"},
			SensitivityCeiling: &ceiling,
		},
	}
}

func fullJudgment() eval.Judgment {
	return eval.Judgment{
		SchemaVersion: eval.SchemaVersion,
		CaseID:        "tasks-exact-001",
		LineageRoot:   "01J8ZKQ4M7:td-f62256",
		Relevance:     eval.Authoritative,
		Required:      true,
		Forbidden:     false,
		Supports:      []string{"state", "title"},
	}
}

func fullRun() eval.Run {
	threshold, observed := 0.9, 0.94
	scores := []eval.CaseScore{
		{
			CaseID: "tasks-exact-001",
			Tags:   []string{"exact"}, SourceFamilies: []string{"tasks"},
			NDCG10: eval.Value{V: 1, OK: true}, Latency: 18 * time.Millisecond,
		},
		{
			CaseID: "docs-lexical-002",
			Tags:   []string{"lexical"}, SourceFamilies: []string{"documents"},
			NDCG10: eval.Value{V: 0.8, OK: true}, Latency: 41 * time.Millisecond, Cold: true,
		},
	}
	return eval.Run{
		SchemaVersion: eval.SchemaVersion,
		RunID:         "20260723-014",
		Status:        eval.StatusPass,
		StartedAt:     time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC),
		FinishedAt:    time.Date(2026, 7, 23, 12, 0, 9, 0, time.UTC),
		Pack: eval.PackRef{
			PackID:      "smoke",
			Version:     "0.1.0",
			ContentHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		Environment: eval.Environment{
			RecallCommit:  "abc1234",
			Dirty:         false,
			ProfileHash:   "sha256:beef",
			Adapters:      []eval.Component{{ID: "recall-stream", Version: "0.1.0"}},
			Indexes:       []eval.Component{{ID: "clara-docs", Version: "gen-000017"}},
			Models:        []eval.Model{{Name: "none", ArtifactHash: ""}},
			OS:            "darwin",
			Arch:          "arm64",
			MemoryBytes:   68719476736,
			CachePolicy:   "cold",
			Warm:          false,
			NetworkAccess: false,
			Seeds:         map[string]int64{"fusion": 1},
		},
		Metrics: eval.ReportOf(scores),
		Gates: []eval.Gate{
			{Name: "abstention_accuracy", Status: eval.GatePass, Threshold: &threshold, Observed: &observed},
			{Name: "no_fixture_mutation", Status: eval.GatePass, Detail: "no fixture file changed"},
		},
	}
}
