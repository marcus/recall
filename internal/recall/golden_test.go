package recall_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/marcus/recall/internal/recall"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The wire shape of these types is a contract every other package and every
// external adapter depends on. Golden files make an accidental rename or
// dropped field a visible diff rather than a silent break.
func TestGoldenWireShapes(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"candidate", fullCandidate()},
		{"manifest", fullManifest()},
		{"health", fullHealth()},
		{"query_request", fullQueryRequest()},
		{"query_response", fullQueryResponse()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')

			path := filepath.Join("testdata", tc.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run: go test ./internal/recall -update)", err)
			}
			if string(got) != string(want) {
				t.Errorf("wire shape changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// Round-tripping a fully populated value proves no field is write-only: a
// field that marshals but does not unmarshal would silently drop data at every
// process boundary.
func TestNoFieldIsLostInRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		value any
		into  func() any
	}{
		{"candidate", fullCandidate(), func() any { return new(recall.Candidate) }},
		{"manifest", fullManifest(), func() any { return new(recall.Manifest) }},
		{"health", fullHealth(), func() any { return new(recall.Health) }},
		{"query_request", fullQueryRequest(), func() any { return new(recall.QueryRequest) }},
		{"query_response", fullQueryResponse(), func() any { return new(recall.QueryResponse) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			target := tc.into()
			if err := json.Unmarshal(first, target); err != nil {
				t.Fatal(err)
			}
			second, err := json.Marshal(reflect.ValueOf(target).Elem().Interface())
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Errorf("round trip lost data.\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

func ts(s string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &parsed
}

func f64(v float64) *float64 { return &v }

func fullCandidate() recall.Candidate {
	return recall.Candidate{
		CandidateID:    "c-0001",
		SourceUID:      "01J8ZKQ4M7",
		SourceID:       "clara-signals",
		SourceRecordID: "sig-8891",
		Locator:        recall.Locator{SourceID: "clara-signals", SourceUID: "01J8ZKQ4M7", Local: "sig-8891"},
		DerivedFrom: []recall.Locator{
			{SourceID: "tasks", Local: "td-f62256"},
		},
		RecordType:     recall.RecordEvent,
		Title:          "Task td-f62256 moved to in_review",
		Excerpt:        "state: open -> in_review",
		LocalRank:      1,
		LocalScore:     f64(12.75),
		MatchSignals:   []recall.MatchSignal{recall.MatchExactIdentifier, recall.MatchLexical},
		ObservedAt:     ts("2026-07-23T12:00:00Z"),
		ConfirmedAt:    ts("2026-07-23T12:00:05Z"),
		EventTime:      ts("2026-07-22T19:30:00Z"),
		ValidFrom:      ts("2026-07-22T19:30:00Z"),
		ValidTo:        ts("2026-07-24T09:00:00Z"),
		SourceRevision: "cursor-4412",
		Sensitivity:    recall.SensitivityInternal,
		Metadata: map[string]any{
			"project": "recall",
			"state":   "in_review",
		},
		ContentFingerprint: "sha256:9f2c",
	}
}

func fullManifest() recall.Manifest {
	return recall.Manifest{
		ProtocolVersion: 1,
		AdapterID:       "recall-stream/0.1.0",
		DisplayName:     "Clara signals",
		RecordTypes:     []recall.RecordType{recall.RecordEvent, recall.RecordMessage},
		QueryModes:      []recall.QueryMode{recall.QueryExact, recall.QueryLexical, recall.QueryTemporal},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessIndexed},
		AsOfSupport:     recall.AsOfFilter,
		DerivesFrom:     "",
		Capabilities: []recall.Capability{
			recall.CapSearch, recall.CapExpand, recall.CapCheckpoint,
		},
		MaxConcurrency:  1,
		FreshnessPolicy: "incremental, cursor checkpointed after durable write",
		Sensitivity:     recall.SensitivityInternal,
		SettingsSchema:  map[string]any{"type": "object"},
	}
}

func fullHealth() recall.Health {
	return recall.Health{
		Status:          recall.HealthDegraded,
		CheckedAt:       *ts("2026-07-23T12:00:00Z"),
		LastSuccess:     ts("2026-07-23T11:58:00Z"),
		SourceWatermark: "cursor-4412",
		IndexWatermark:  "cursor-4390",
		IndexGeneration: "gen-000017",
		IndexModel:      "none",
		RecordCount:     18422,
		IndexedCount:    18390,
		FailedCount:     3,
		Coverage:        recall.IndexPartial,
		Diagnostics:     map[string]any{"lag_records": float64(32)},
		ColdStart:       42 * time.Millisecond,
	}
}

func fullQueryRequest() recall.QueryRequest {
	return recall.QueryRequest{
		Query:   "what is the state of td-f62256?",
		Profile: "work",
		Scope: &recall.Scope{
			SourceIDs:   []string{"tasks", "clara-signals"},
			RecordTypes: []recall.RecordType{recall.RecordTask, recall.RecordEvent},
			Since:       ts("2026-07-01T00:00:00Z"),
			Until:       ts("2026-07-24T00:00:00Z"),
		},
		AsOf:             ts("2026-07-23T12:00:00Z"),
		Context:          []string{"we were discussing the recall epic"},
		ConversationID:   "conv-7",
		RequestID:        "req-31",
		SuppressLineages: []recall.LineageRoot{"01J8ZKQ4M7:sig-8000"},
		Mode:             recall.ModePreReply,
		Budget:           recall.Budget{LatencyMS: 250, ResponseTokens: 1200},
		Limit:            10,
	}
}

func fullQueryResponse() recall.QueryResponse {
	cand := fullCandidate()
	return recall.QueryResponse{
		Results: []recall.Result{{
			Primary: cand,
			Members: []recall.ClusterMember{{
				LineageRoot: "01J8ZKQ4M7:sig-8891",
				Candidates:  []recall.Candidate{cand},
			}},
			Explanation: recall.Explanation{
				SourceUID:     "01J8ZKQ4M7",
				SourceID:      "clara-signals",
				LocalRank:     1,
				LocalPoolSize: 8,
				MatchSignals:  []recall.MatchSignal{recall.MatchExactIdentifier},
				Prior: recall.PriorExplanation{
					Base: 1.0, Intent: 0.25, Rule: "identifier_query", Effective: 1.25,
				},
				LineageRoot: "01J8ZKQ4M7:sig-8891",
				Corroboration: recall.CorroborationExplanation{
					IndependentUnits: 2,
					Sources:          []string{"tasks", "clara-signals"},
					Cap:              2.0,
					CapApplied:       false,
				},
				Freshness: recall.FreshnessExplanation{
					Mode:            recall.FreshnessIndexed,
					SourceRevision:  "cursor-4412",
					IndexGeneration: "gen-000017",
					IndexModel:      "none",
					ObservedAt:      ts("2026-07-23T12:00:00Z"),
					ConfirmedAt:     ts("2026-07-23T12:00:05Z"),
					AsOfHonored:     recall.AsOfFilter,
				},
				Reranker:      recall.RerankerExplanation{Used: false},
				ExactPromoted: true,
				Score:         0.0328,
				RankConstant:  60,
			},
			Score: 0.0328,
		}},
		SourceOutcomes: []recall.SourceReport{
			{
				SourceUID: "01J8ZKQ4M7", SourceID: "clara-signals",
				Outcome: recall.SearchSuccess, Candidates: 8,
				Elapsed: 18 * time.Millisecond, SourceWatermark: "cursor-4412",
				IndexGeneration: "gen-000017", ConfirmedAt: ts("2026-07-23T12:00:05Z"),
			},
			{
				SourceUID: "01J8ZKQ4M8", SourceID: "clara-docs",
				Outcome: recall.SearchSkipped, Reason: "as_of_unsupported",
			},
		},
		Plan: recall.Plan{
			Profile: "work",
			Sources: []recall.PlanSource{
				{SourceUID: "01J8ZKQ4M7", SourceID: "clara-signals", Eligible: true, Limit: 20, Timeout: 200 * time.Millisecond, Prior: 1.25},
				{SourceUID: "01J8ZKQ4M8", SourceID: "clara-docs", Eligible: false, Reason: "as_of_unsupported"},
			},
			Deadline:       *ts("2026-07-23T12:00:00Z"),
			Reserve:        25 * time.Millisecond,
			Limit:          10,
			RankConst:      60,
			Corrobor:       2.0,
			RelevanceFloor: 0.05,
		},
		Suppressed: []recall.Suppression{
			{Reason: recall.SuppressLineageSeen, Count: 1, LineageRoot: "01J8ZKQ4M7:sig-8000"},
			{Reason: recall.SuppressRelevanceFloor, Count: 4},
		},
		Outcome:        recall.OutcomeAnswered,
		Coverage:       recall.CoverageDegraded,
		Truncated:      true,
		DroppedResults: 3,
		Elapsed:        63 * time.Millisecond,
	}
}
