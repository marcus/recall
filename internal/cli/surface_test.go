package cli_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/recall"
)

type transportCore struct {
	query   recall.QueryResponse
	expand  recall.ExpandResponse
	sources api.Listing
	doctor  api.Listing
}

func (c *transportCore) Query(context.Context, recall.QueryRequest) (recall.QueryResponse, error) {
	return c.query, nil
}
func (c *transportCore) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	return c.expand, nil
}
func (c *transportCore) Sources(context.Context) (api.Listing, error) { return c.sources, nil }
func (c *transportCore) Doctor(context.Context) (api.Listing, error)  { return c.doctor, nil }
func (c *transportCore) Profile() string                              { return "work" }

func TestCLIInProcessAndAgainstServeAreIdentical(t *testing.T) {
	core := &transportCore{
		query: recall.QueryResponse{
			Results: []recall.Result{{
				Primary: recall.Candidate{
					CandidateID: "c-1", SourceUID: "01UID", SourceID: "notes",
					SourceRecordID: "record", Locator: recall.Locator{SourceID: "notes", Local: "record"},
					RecordType: recall.RecordDocument, Title: "Title", Excerpt: "Excerpt", LocalRank: 1,
				},
				Score: 0.5,
			}},
			SourceOutcomes: []recall.SourceReport{{
				SourceUID: "01UID", SourceID: "notes", Outcome: recall.SearchSuccess, Candidates: 1,
			}},
			Plan:     recall.Plan{Profile: "work", Limit: 10},
			Outcome:  recall.OutcomeAnswered,
			Coverage: recall.CoverageComplete,
		},
		expand: recall.ExpandResponse{
			Content: "Full evidence", Provenance: "notes.md#L1", SourceRevision: "abc",
		},
		sources: api.Listing{Status: api.StatusOK, Payload: map[string]any{
			"profile": "work", "max_sensitivity": "internal",
			"sources": []any{},
		}},
		doctor: api.Listing{Status: api.StatusDegraded, Payload: map[string]any{
			"status": "degraded", "profile": "work", "checks": []any{},
			"failed_checks": 0, "degraded_checks": 1,
		}},
	}
	server := httptest.NewServer(api.NewHandler(api.ServerOptions{Core: core, BearerToken: "secret"}))
	defer server.Close()

	tests := []struct {
		name string
		args []string
	}{
		{"query", []string{"query", "--json", "--profile", "work", "decision"}},
		{"expand", []string{"expand", "--json", "--profile", "work", "notes:record"}},
		{"sources", []string{"sources", "--json", "--profile", "work"}},
		{"doctor", []string{"doctor", "--json", "--profile", "work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			localCode, localOut, localErr := runSurfaceCLI(t, core, nil, tc.args...)
			remoteArgs := append([]string{}, tc.args...)
			remoteArgs = append(remoteArgs, "--server", server.URL, "--auth-token-env", "RECALL_TEST_TOKEN")
			remoteCode, remoteOut, remoteErr := runSurfaceCLI(t, nil,
				func(name string) (string, bool) {
					if name == "RECALL_TEST_TOKEN" {
						return "secret", true
					}
					return "", false
				}, remoteArgs...)

			if localCode != remoteCode || localOut != remoteOut || localErr != remoteErr {
				t.Fatalf("surface mismatch\nlocal:  code=%d stdout=%q stderr=%q\nremote: code=%d stdout=%q stderr=%q",
					localCode, localOut, localErr, remoteCode, remoteOut, remoteErr)
			}
		})
	}
}

func TestMCPCommandUsesTheSameInjectedCore(t *testing.T) {
	core := &transportCore{}
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), cli.Env{
		Args:   []string{"mcp"},
		Stdin:  bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Core:   core,
	})
	if code != cli.ExitOK {
		t.Fatalf("mcp exit=%d stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); got != "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n" {
		t.Fatalf("unexpected MCP output: %q", got)
	}
}

func TestServeRefusesUnsafeBindBeforeOpeningSocket(t *testing.T) {
	core := &transportCore{}
	for _, tc := range []struct {
		name   string
		args   []string
		lookup func(string) (string, bool)
		want   string
	}{
		{
			name: "non-loopback without auth",
			args: []string{"serve", "--addr", "192.0.2.10:8765"},
			want: "requires authentication",
		},
		{
			name: "named token is absent",
			args: []string{"serve", "--addr", "192.0.2.10:8765", "--auth-token-env", "MISSING"},
			want: "MISSING is unset or empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runSurfaceCLI(t, core, tc.lookup, tc.args...)
			if code != cli.ExitError {
				t.Fatalf("serve exit=%d stderr=%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr does not contain %q: %s", tc.want, stderr)
			}
		})
	}
}

func runSurfaceCLI(
	t *testing.T,
	core api.Core,
	lookup func(string) (string, bool),
	args ...string,
) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), cli.Env{
		Args:      args,
		Stdout:    &stdout,
		Stderr:    &stderr,
		Core:      core,
		LookupEnv: lookup,
	})
	return code, stdout.String(), stderr.String()
}
