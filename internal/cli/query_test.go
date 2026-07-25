package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// A script has to be able to tell "nothing matched" from "the sources were
// down". These four situations are the whole vocabulary, and each one has to
// reach a different exit code without the caller parsing any output.
func TestQueryExitCodes(t *testing.T) {
	tests := []struct {
		name  string
		tune  func(docs, tasks *fake)
		want  int
		state string
	}{
		{
			name: "answered",
			tune: func(docs, _ *fake) {
				docs.candidates = []recall.Candidate{candidate("a.md", 1)}
			},
			want:  cli.ExitOK,
			state: "results and every eligible source answered",
		},
		{
			name:  "abstained",
			tune:  func(*fake, *fake) {},
			want:  cli.ExitAbstained,
			state: "nothing matched, but both sources looked",
		},
		{
			name: "degraded",
			tune: func(docs, tasks *fake) {
				docs.candidates = []recall.Candidate{candidate("a.md", 1)}
				tasks.searchErr = protocol.ErrSourceUnavailable
			},
			want:  cli.ExitDegraded,
			state: "answered, but from an incomplete set of sources",
		},
		{
			name: "failed",
			tune: func(docs, tasks *fake) {
				docs.searchErr = protocol.ErrSourceUnavailable
				tasks.searchErr = protocol.ErrSourceUnavailable
			},
			want:  cli.ExitFailed,
			state: "no source answered, so no-results is a claim nothing supports",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docs, tasks := &fake{manifest: manifest()}, &fake{manifest: manifest()}
			tc.tune(docs, tasks)
			h := newHarness(t, harnessOptions{
				userTOML: twoSourceTOML,
				adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
			})

			code, stdout, stderr := h.run("query", "anything")
			if code != tc.want {
				t.Errorf("exit = %d, want %d (%s)\n%s%s", code, tc.want, tc.state, stdout, stderr)
			}
		})
	}
}

// A degraded run that also abstained exits degraded. The abstention is a claim
// about the corpus, and an incomplete set of sources does not support one.
func TestAbstainingWithADeadSourceExitsDegraded(t *testing.T) {
	docs := &fake{manifest: manifest()}
	tasks := &fake{manifest: manifest(), searchErr: protocol.ErrSourceUnavailable}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	code, stdout, _ := h.run("query", "anything")
	if code != cli.ExitDegraded {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitDegraded, stdout)
	}
}

// The failure mode this command exists to prevent: a person running a query
// without --json being shown a partial answer as though it were a whole one.
func TestDegradedCoverageIsVisibleInHumanOutput(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	tasks := &fake{manifest: manifest(), searchErr: protocol.ErrSourceUnavailable}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	_, stdout, _ := h.run("query", "anything")
	if strings.Contains(stdout, "--json") {
		t.Fatal("human output should not need --json to tell the truth")
	}
	contains(t, stdout, "coverage degraded", "the header states coverage inline")
	contains(t, stdout, "degraded coverage: tasks (unreachable)",
		"the source that could not answer is named, not silently absent")
	contains(t, stdout, "tasks (01UIDTASKS)", "the failed source still appears in the source list")
}

// Neither surface may carry a fact the other lacks. The tiered text is derived
// from the same structure the JSON serializes, so a fact that reaches one and
// not the other means a surface acquired a view of its own.
func TestHumanAndJSONCarryTheSameFacts(t *testing.T) {
	observed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	docs := &fake{
		manifest: manifest(),
		health: recall.Health{
			Status:          recall.HealthHealthy,
			Coverage:        recall.IndexComplete,
			SourceWatermark: "w-000123",
			IndexGeneration: "gen-000042",
			IndexModel:      "lexical",
			IndexConfig:     "bm25-k1.2",
		},
		candidates: []recall.Candidate{
			candidate("a.md", 1, func(c *recall.Candidate) {
				c.ObservedAt = &observed
				c.SourceRevision = "rev-9"
				c.ContentFingerprint = "fp-1"
			}),
		},
	}
	// Denied by the ceiling and unreachable: every kind of source report has to
	// appear in both surfaces, not only the successful kind.
	tasks := &fake{manifest: manifest(), searchErr: protocol.ErrSourceUnavailable}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	_, human, _ := h.run("query", "--explain", "ranking")
	_, machine, _ := h.run("query", "--json", "ranking")

	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(machine), &resp); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, machine)
	}

	// Every fact below is read off the parsed JSON, so the table cannot drift
	// from what the machine surface actually said.
	facts := []struct {
		what string
		want string
	}{
		{"outcome", string(resp.Outcome)},
		{"coverage", string(resp.Coverage)},
		{"result locator", resp.Results[0].Primary.Locator.String()},
		{"result title", resp.Results[0].Primary.Title},
		{"result excerpt", resp.Results[0].Primary.Excerpt},
		{"record type", string(resp.Results[0].Primary.RecordType)},
		{"source record id", resp.Results[0].Primary.SourceRecordID},
		{"candidate id", resp.Results[0].Primary.CandidateID},
		{"source revision", resp.Results[0].Primary.SourceRevision},
		{"content fingerprint", resp.Results[0].Primary.ContentFingerprint},
		{"observed_at", resp.Results[0].Primary.ObservedAt.UTC().Format(time.RFC3339)},
		{"lineage root", string(resp.Results[0].Explanation.LineageRoot)},
		{"source uid", string(resp.Results[0].Explanation.SourceUID)},
		{"index generation", resp.Results[0].Explanation.Freshness.IndexGeneration},
		{"index config", resp.Results[0].Explanation.Freshness.IndexConfig},
		{"freshness mode", string(resp.Results[0].Explanation.Freshness.Mode)},
		{"plan profile", resp.Plan.Profile},
		{"rank constant", "60"},
		{"corroboration cap", "2"},
		{"failed source id", "tasks"},
		{"failed source uid", "01UIDTASKS"},
		{"failure reason", "unreachable"},
	}
	for _, f := range facts {
		if f.want == "" {
			t.Fatalf("the JSON response carries no %s; the fixture no longer exercises it", f.what)
		}
		if !strings.Contains(human, f.want) {
			t.Errorf("human output is missing the %s %q that JSON carries\n--- human ---\n%s",
				f.what, f.want, human)
		}
	}
}

// The score explanation is the product surface for ranking decisions, and it is
// rendered by internal/explain so the CLI cannot grow a second opinion about
// what a score means.
func TestExplainRendersTheStructuredExplanation(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})

	_, plain, _ := h.run("query", "anything")
	if strings.Contains(plain, "corroboration:") {
		t.Error("the explanation block appeared without --explain")
	}
	_, explained, _ := h.run("query", "--explain", "anything")
	for _, want := range []string{"source:", "local rank:", "prior:", "corroboration:", "score:"} {
		contains(t, explained, want, "explain.Render's block is what --explain shows")
	}
}

// Scope is a hard eligibility constraint, so an unreadable one fails the
// command. Ignoring it would search sources the caller believed it excluded.
func TestBadArgumentsFail(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no query text", []string{"query"}, "no query text"},
		{"unknown scope key", []string{"query", "--scope", "colour=red", "x"}, "scope key"},
		{"malformed scope", []string{"query", "--scope", "source", "x"}, "want key=value"},
		{"bad as-of", []string{"query", "--as-of", "yesterday", "x"}, "RFC 3339"},
		{"unknown flag", []string{"query", "--nope", "x"}, "flag provided but not defined"},
		{"bad detail", []string{"expand", "--detail", "everything", "docs:a.md"}, "want summary"},
		{"expand takes one locator", []string{"expand"}, "exactly one locator"},
		{"unknown command", []string{"frobnicate"}, "unknown command"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{
				userTOML: twoSourceTOML,
				adapters: fakeAdapters(map[string]*fake{
					"fakedocs": {manifest: manifest()}, "faketasks": {manifest: manifest()},
				}),
			})
			code, _, stderr := h.run(tc.args...)
			if code != cli.ExitError {
				t.Errorf("exit = %d, want %d", code, cli.ExitError)
			}
			contains(t, stderr, tc.want, "a rejected argument says what was wrong with it")
		})
	}
}

// A source that cannot honor a historical boundary is excluded and reported,
// and coverage becomes degraded. The CLI must pass --as-of through rather than
// deciding anything about it.
func TestAsOfReachesEligibility(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	noHistory := manifest()
	noHistory.AsOfSupport = recall.AsOfNone
	tasks := &fake{manifest: noHistory}

	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	code, stdout, _ := h.run("query", "--as-of", "2026-01-01T00:00:00Z", "anything")
	if code != cli.ExitDegraded {
		t.Errorf("exit = %d, want degraded\n%s", code, stdout)
	}
	contains(t, stdout, "as_of_unsupported", "the excluded source and its reason are reported")
}

// --scope source= narrows the plan, and a source left out because the caller
// scoped it away is the configured system working as asked: not degraded.
func TestScopeNarrowsThePlanWithoutDegrading(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	tasks := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("td-1", 1)}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})

	code, stdout, _ := h.run("query", "--scope", "source=docs", "anything")
	if code != cli.ExitOK {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitOK, stdout)
	}
	contains(t, stdout, "out_of_scope", "the scoped-away source is reported, not hidden")
	if strings.Contains(stdout, "td-1") {
		t.Error("a scoped-away source contributed results")
	}
}

func TestProjectAndEntityScopeReachAdapters(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	tasks := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("td-1", 1)}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})
	code, _, stderr := h.run("query", "--scope", "project=recall,entity=Marcus", "decision")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for name, f := range map[string]*fake{"docs": docs, "tasks": tasks} {
		if f.lastSearch.Filters.Project != "recall" ||
			len(f.lastSearch.Filters.Entities) != 1 ||
			f.lastSearch.Filters.Entities[0] != "Marcus" {
			t.Errorf("%s saw filters %+v, want project and entity", name, f.lastSearch.Filters)
		}
	}
}

// Every withheld candidate is counted with a reason, so a person can be told
// something was not shown without being told what it was.
func TestSuppressionIsReportedInBothSurfaces(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{
		candidate("public.md", 1),
		candidate("sealed.md", 2, func(c *recall.Candidate) {
			c.Sensitivity = recall.SensitivityRestricted
		}),
	}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})

	_, human, _ := h.run("query", "anything")
	_, machine, _ := h.run("query", "--json", "anything")

	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(machine), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Suppressed) == 0 {
		t.Fatal("a record above the ceiling was dropped without being counted")
	}
	contains(t, human, "suppressed", "the human surface says something was withheld")
	contains(t, human, resp.Suppressed[0].Reason, "and why")
	if strings.Contains(human, "sealed.md") {
		t.Error("the withheld record was named")
	}
}

// --limit is the caller's, and dropping trailing results is truncation, which
// is a budget fact and never degraded coverage.
func TestLimitTruncatesWithoutDegrading(t *testing.T) {
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{
		candidate("a.md", 1), candidate("b.md", 2), candidate("c.md", 3),
	}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})

	code, stdout, _ := h.run("query", "--limit", "1", "--json", "anything")
	if code != cli.ExitOK {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitOK, stdout)
	}
	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("results = %d, want 1", len(resp.Results))
	}
	if !resp.Truncated || resp.DroppedResults == 0 {
		t.Errorf("truncation was not reported: truncated=%v dropped=%d", resp.Truncated, resp.DroppedResults)
	}
	if resp.Coverage != recall.CoverageComplete {
		t.Errorf("coverage = %s; truncation is not degradation", resp.Coverage)
	}
}
