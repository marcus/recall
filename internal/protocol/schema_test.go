package protocol_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

func schemas(t *testing.T) *protocol.SchemaSet {
	t.Helper()
	set, err := protocol.Schemas()
	if err != nil {
		t.Fatalf("compile embedded schemas: %v", err)
	}
	return set
}

// The schemas are the contract, so the Go types must satisfy them exactly. A
// field renamed in internal/recall without a schema change would otherwise ship
// as an adapter that silently fails validation in the field.
func TestGoTypesSatisfyTheirSchemas(t *testing.T) {
	set := schemas(t)
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name   string
		method string
		result bool
		value  any
	}{
		{
			name: "initialize params", method: protocol.MethodInitialize,
			value: protocol.InitializeParams{
				ProtocolVersionMin: 1, ProtocolVersionMax: 1,
				Workdir:  "/state/recall/work/tasks",
				SourceID: "tasks",
				Location: "/home/u/tasks.db",
				Settings: map[string]any{"read_only": true},
			},
		},
		{
			name: "manifest", method: protocol.MethodInitialize, result: true,
			value: recall.Manifest{
				ProtocolVersion: 1,
				AdapterID:       "recall-tasks/0.1.0",
				DisplayName:     "Tasks",
				RecordTypes:     []recall.RecordType{recall.RecordTask},
				QueryModes:      []recall.QueryMode{recall.QueryExact, recall.QueryLexical},
				FreshnessModes:  []recall.FreshnessMode{recall.FreshnessLive},
				AsOfSupport:     recall.AsOfNone,
				Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
				MaxConcurrency:  1,
				Sensitivity:     recall.SensitivityInternal,
			},
		},
		{
			name: "search params", method: protocol.MethodSearch,
			value: recall.SearchRequest{
				Query: "state of td-f62256",
				Filters: recall.Filters{
					RecordTypes: []recall.RecordType{recall.RecordTask},
					Entities:    []string{"Marcus"},
					Project:     "recall",
				},
				Limit:    20,
				Deadline: now,
			},
		},
		{
			name: "skipped search result", method: protocol.MethodSearch, result: true,
			value: recall.SearchResponse{
				Candidates:  []recall.Candidate{},
				Diagnostics: map[string]any{"unsupported_filters": []string{"project"}},
				Outcome:     recall.SearchSkipped,
				Reason:      recall.SkipFilterUnsupported,
			},
		},
		{
			name: "search result", method: protocol.MethodSearch, result: true,
			value: recall.SearchResponse{
				Candidates: []recall.Candidate{{
					CandidateID:    "c-1",
					SourceRecordID: "td-f62256",
					Locator:        recall.Locator{SourceID: "tasks", Local: "td-f62256"},
					RecordType:     recall.RecordTask,
					Title:          "Ship the adapter protocol",
					LocalRank:      1,
					MatchSignals:   []recall.MatchSignal{recall.MatchExactIdentifier},
					Sensitivity:    recall.SensitivityInternal,
				}},
				Outcome: recall.SearchSuccess,
			},
		},
		{
			name: "expand params", method: protocol.MethodExpand,
			value: recall.ExpandRequest{
				Locator:  recall.Locator{SourceID: "tasks", Local: "td-f62256"},
				Detail:   recall.DetailFull,
				Budget:   4096,
				Deadline: now,
			},
		},
		{
			name: "expand result", method: protocol.MethodExpand, result: true,
			value: recall.ExpandResponse{Content: "body", Truncated: false},
		},
		{
			name: "health params", method: protocol.MethodHealth,
			value: protocol.HealthParams{Deadline: now},
		},
		{
			name: "health result", method: protocol.MethodHealth, result: true,
			value: recall.Health{
				Status:    recall.HealthHealthy,
				CheckedAt: now,
				Coverage:  recall.IndexComplete,
				ColdStart: 42 * time.Millisecond,
			},
		},
		{
			name: "shutdown params", method: protocol.MethodShutdown,
			value: protocol.ShutdownParams{},
		},
		{
			name: "cancel params", method: protocol.MethodCancel,
			value: protocol.CancelParams{ID: protocol.NumberID(7)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if tt.result {
				err = set.ValidateResult(tt.method, raw)
			} else {
				err = set.ValidateParams(tt.method, raw)
			}
			if err != nil {
				t.Fatalf("%s\n%v", raw, err)
			}
		})
	}
}

// Each of these is a specific way a contract can be broken. Rejecting them is
// the reason validation happens on both sides rather than at the consumer.
func TestSchemasRejectContractBreaks(t *testing.T) {
	set := schemas(t)

	tests := []struct {
		name    string
		method  string
		result  bool
		payload string
	}{
		{
			name: "candidate claiming its own identity", method: protocol.MethodSearch, result: true,
			// source_uid is attached by the core. An adapter that names one is
			// claiming an identity configuration did not give it.
			payload: `{"outcome":"success","candidates":[{"candidate_id":"c","source_uid":"01J8ZK",
				"source_record_id":"r","locator":"tasks:td-1","record_type":"task","title":"t",
				"local_rank":1,"sensitivity":"internal"}]}`,
		},
		{
			name: "search result without an outcome", method: protocol.MethodSearch, result: true,
			// Without an outcome an unreachable source is indistinguishable
			// from a source with no matches.
			payload: `{"candidates":[]}`,
		},
		{
			name: "local_rank of zero", method: protocol.MethodSearch, result: true,
			// Local rank is one-based and mandatory; zero means the adapter did
			// not rank at all.
			payload: `{"outcome":"success","candidates":[{"candidate_id":"c","source_record_id":"r",
				"locator":"tasks:td-1","record_type":"task","title":"t","local_rank":0,
				"sensitivity":"internal"}]}`,
		},
		{
			name: "locator without a source", method: protocol.MethodExpand,
			// A bare local part is uninterpretable outside the adapter.
			payload: `{"locator":"td-1","detail":"full","budget_bytes":10,"deadline":"2026-07-24T12:00:00Z"}`,
		},
		{
			name: "health without coverage", method: protocol.MethodHealth, result: true,
			// A recent timestamp alone is not health; coverage is what makes a
			// partial source visible.
			payload: `{"status":"healthy","checked_at":"2026-07-24T12:00:00Z"}`,
		},
		{
			name: "health status outside the vocabulary", method: protocol.MethodHealth, result: true,
			payload: `{"status":"ok","checked_at":"2026-07-24T12:00:00Z","coverage":"complete"}`,
		},
		{
			name: "checked_at that is not an instant", method: protocol.MethodHealth, result: true,
			payload: `{"status":"healthy","checked_at":"yesterday","coverage":"complete"}`,
		},
		{
			name: "handshake without a workdir", method: protocol.MethodInitialize,
			// An adapter with no writable directory has nowhere legitimate to
			// put an index.
			payload: `{"protocol_version_min":1,"protocol_version_max":1}`,
		},
		{
			name: "unknown field in a manifest", method: protocol.MethodInitialize, result: true,
			payload: `{"protocol_version":1,"adapter_id":"a","display_name":"A","record_types":["task"],
				"query_modes":["exact"],"freshness_modes":["live"],"as_of_support":"none",
				"capabilities":["search"],"sensitivity":"internal","source_uid":"01J8ZK"}`,
		},
		{
			name: "search without a deadline", method: protocol.MethodSearch,
			// Every request carries a deadline; without one nothing bounds it.
			payload: `{"query":"q","filters":{},"limit":10}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.result {
				err = set.ValidateResult(tt.method, json.RawMessage(tt.payload))
			} else {
				err = set.ValidateParams(tt.method, json.RawMessage(tt.payload))
			}
			if err == nil {
				t.Fatal("schema accepted a payload that breaks the contract")
			}
		})
	}
}

// Diagnostics must not carry source content. A validation failure names where
// the payload broke, never what it contained.
func TestValidationErrorsReportPlacesNotValues(t *testing.T) {
	set := schemas(t)
	const secret = "the patient's diagnosis"
	payload := `{"outcome":"success","candidates":[{"candidate_id":"c","source_record_id":"r",
		"locator":"tasks:td-1","record_type":"task","title":"` + secret + `","local_rank":0,
		"sensitivity":"internal"}]}`

	err := set.ValidateResult(protocol.MethodSearch, json.RawMessage(payload))
	if err == nil {
		t.Fatal("expected a validation failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error leaked instance content: %v", err)
	}
	if !strings.Contains(err.Error(), "/candidates/0/local_rank") {
		t.Fatalf("validation error should name the failing location, got %v", err)
	}
}

func TestUnknownMethodHasNoContract(t *testing.T) {
	set := schemas(t)
	err := set.ValidateParams("recall/summarize", json.RawMessage(`{}`))
	if !errors.Is(err, protocol.ErrMethodNotFound) {
		t.Fatalf("err = %v, want method_not_found", err)
	}
}
