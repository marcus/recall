package tasks_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// TestTypedMetadataSurvives is the structured-source obligation from
// docs/spec.md: "Typed fields are preserved in candidate metadata; flattening a
// person or task into anonymous text discards ranking and routing signal."
//
// The types matter as much as the presence. A boolean that arrived as the
// string "false", or a list flattened to "a, b", would satisfy a
// key-existence check and still be useless to anything routing on it.
func TestTypedMetadataSurvives(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := search(t, a, "vendor renewal quote")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	candidate := find(t, resp.Candidates, "aaaa0005")
	meta := candidate.Metadata

	// The recorded record: TODO, priority B, @email, a deadline with a time of
	// day, filed under the "Tasks" area.
	want := map[string]any{
		"state":               "TODO",
		"priority":            "B",
		"contexts":            []string{"@email"},
		"deadline":            "2026-01-20",
		"deadline_at":         "2026-01-20T17:30:00Z",
		"project":             "Tasks",
		"available":           true,
		"availability_reason": "available",
		"deferred":            false,
		"store":               "live",
	}
	for key, expected := range want {
		got, ok := meta[key]
		if !ok {
			t.Errorf("metadata is missing %q; a structured source must not drop typed fields", key)
			continue
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("metadata[%q] = %#v (%T), want %#v (%T)", key, got, got, expected, expected)
		}
	}

	// Absent is absent. An unset field must not appear as an empty string, or
	// "no recurrence" and "recurrence unknown" become the same value.
	for _, key := range []string{"recur", "closed", "scheduled"} {
		if _, present := meta[key]; present {
			t.Errorf("metadata[%q] is present on a record that has no such value", key)
		}
	}

	if candidate.EventTime == nil {
		t.Fatal("event_time is nil; the task has a deadline")
	}
	if want := time.Date(2026, 1, 20, 17, 30, 0, 0, time.UTC); !candidate.EventTime.Equal(want) {
		t.Errorf("event_time = %v, want %v (the CLI's derived instant, not a guessed zone)",
			candidate.EventTime, want)
	}
	if candidate.SourceRecordID != "aaaa0005" {
		t.Errorf("source_record_id = %q, want the task id", candidate.SourceRecordID)
	}
	if got := candidate.Locator.String(); got != "tasks:aaaa0005" {
		t.Errorf("locator = %q, want tasks:aaaa0005", got)
	}
	if candidate.RecordType != recall.RecordTask {
		t.Errorf("record_type = %q, want task", candidate.RecordType)
	}
}

// TestTypedMetadataOnRecurringRecord covers the fields the vendor task does
// not have, so no field is asserted only by its absence somewhere else.
func TestTypedMetadataOnRecurringRecord(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := search(t, a, "water office plants")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	meta := find(t, resp.Candidates, "aaaa0008").Metadata

	if got := meta["recur"]; got != ".+1w" {
		t.Errorf("metadata[recur] = %#v, want the cookie string %q", got, ".+1w")
	}
	if got := meta["scheduled"]; got != "2026-01-13" {
		t.Errorf("metadata[scheduled] = %#v, want 2026-01-13", got)
	}
	if got, ok := meta["contexts"].([]string); !ok || !slices.Contains(got, "@office") {
		t.Errorf("metadata[contexts] = %#v, want a list containing @office", meta["contexts"])
	}
}

// TestLocalRankIsDenseAndOrdered pins the one mandatory relevance signal.
// Fusion consumes local_rank and nothing else, so a gap or a repeat here is a
// wrong answer upstream, not a cosmetic problem.
func TestLocalRankIsDenseAndOrdered(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := search(t, a, "site copy generator")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) < 2 {
		t.Fatalf("want several candidates, got %d", len(resp.Candidates))
	}
	for i, c := range resp.Candidates {
		if c.LocalRank != i+1 {
			t.Fatalf("candidate %d has local_rank %d, want %d", i, c.LocalRank, i+1)
		}
	}
}

// TestRankingIsDeterministic is what makes evaluation transcripts diffable: the
// same store and query must produce the same order, including among candidates
// that score identically.
func TestRankingIsDeterministic(t *testing.T) {
	first := order(t, "site copy generator budget")
	for i := range 5 {
		if got := order(t, "site copy generator budget"); !slices.Equal(got, first) {
			t.Fatalf("run %d ordered %v, first run ordered %v", i, got, first)
		}
	}
}

func order(t *testing.T, query string) []string {
	t.Helper()
	a := newAdapter(t, recordedStore(t), nil)
	resp, err := search(t, a, query)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ids := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		ids = append(ids, c.SourceRecordID)
	}
	return ids
}

// TestBodyMatchesAreRecalled covers the reason the adapter spends an extra
// invocation per term: the bulk list shape carries no note text, so without
// the `--body` probe a task whose evidence lives only in its notes would be
// invisible to lexical search.
func TestBodyMatchesAreRecalled(t *testing.T) {
	cli := recordedStore(t)
	// "landing" appears in aaaa000c's title and in aaaa000b's... nothing. Use
	// the recorded body probe: /site returns aaaa000b, whose title also
	// contains "site", so assert the probe was issued and its ids were used.
	a := newAdapter(t, cli, nil)

	if _, err := search(t, a, "site"); err != nil {
		t.Fatalf("search: %v", err)
	}
	var probed bool
	for _, call := range cli.invocations() {
		if call[0] == "list" && hasArg(call, "--body") && hasArg(call, "/site") {
			probed = true
		}
	}
	if !probed {
		t.Error("no --body probe was issued; body text can only reach ranking through one")
	}
}

// TestTermProbesAreCapped keeps process count a function of configuration
// rather than of how much text a caller pasted into the query.
func TestTermProbesAreCapped(t *testing.T) {
	cli := recordedStore(t)
	a := newAdapter(t, cli, map[string]any{"max_term_probes": 2})

	long := "vendor renewal quote budget finance planning outline generator landing"
	if _, err := search(t, a, long); err != nil {
		t.Fatalf("search: %v", err)
	}

	probes := 0
	for _, call := range cli.invocations() {
		if call[0] == "list" && hasArg(call, "--body") {
			probes++
		}
	}
	if probes != 2 {
		t.Errorf("issued %d body probes for a nine-term query, want the configured cap of 2", probes)
	}
}

// TestDiagnosticsReportInvocationCost is the requirement that this design's
// cost be measured rather than assumed: the CLI is one-shot, so a search pays
// for process spawns and has to say how many and how long.
func TestDiagnosticsReportInvocationCost(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := search(t, a, "vendor renewal")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	invocations, ok := resp.Diagnostics["cli_invocations"].(int)
	if !ok || invocations < 2 {
		t.Errorf("cli_invocations = %#v, want at least the listing and the project rollup",
			resp.Diagnostics["cli_invocations"])
	}
	for _, key := range []string{"cli_wall_ms", "cli_process_ms", "query_mode", "corpus_records", "scope"} {
		if _, present := resp.Diagnostics[key]; !present {
			t.Errorf("diagnostics is missing %q", key)
		}
	}
	if resp.SourceWatermark == "" {
		t.Error("no source_watermark; a live source must say which revision it read")
	}
	if resp.Candidates[0].SourceRevision != resp.SourceWatermark {
		t.Error("candidate source_revision disagrees with the search watermark")
	}
}

// TestSettingsScopeTheInstance covers the configured field filters. They are
// instance policy, so a source scoped to open @work items must not answer with
// a closed personal one however well it matches.
func TestSettingsScopeTheInstance(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		query    string
		wantIDs  []string
		why      string
	}{
		{
			name:     "state filter",
			settings: map[string]any{"states": []any{"NEXT"}},
			query:    "planning outline generator static",
			wantIDs:  []string{"aaaa0004", "aaaa000b"},
			why:      "only NEXT tasks survive, though TODO tasks match the terms too",
		},
		{
			name:     "priority filter",
			settings: map[string]any{"priorities": []any{"A"}},
			query:    "planning outline quote",
			wantIDs:  []string{"aaaa0004"},
			why:      "the vendor task is priority B",
		},
		{
			name:     "priority none",
			settings: map[string]any{"priorities": []any{"none"}},
			query:    "sourdough",
			wantIDs:  []string{"aaaa000e"},
			why:      "an unset priority is selectable, not unfilterable",
		},
		{
			name:     "tag filter",
			settings: map[string]any{"tags": []any{"important"}},
			query:    "planning outline sourdough",
			wantIDs:  []string{"aaaa0004"},
			why:      "tags and contexts are separate facets in the store",
		},
		{
			name:     "context filter without the sigil",
			settings: map[string]any{"contexts": []any{"email"}},
			query:    "vendor sourdough",
			wantIDs:  []string{"aaaa0005"},
			why:      "a configured context matches whether or not it is written with @",
		},
		{
			name:     "done scope",
			settings: map[string]any{"scope": "done"},
			query:    "expense report",
			wantIDs:  []string{"aaaa0007"},
			why:      "the done scope reaches closed work the open listing hides",
		},
		{
			name:     "done scope hides open work",
			settings: map[string]any{"scope": "done"},
			query:    "vendor renewal quote",
			wantIDs:  nil,
			why:      "an open task must not leak into a closed-only instance",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t, recordedStore(t), tc.settings)
			resp, err := search(t, a, tc.query)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			got := make([]string, 0, len(resp.Candidates))
			for _, c := range resp.Candidates {
				got = append(got, c.SourceRecordID)
			}
			slices.Sort(got)
			want := slices.Clone(tc.wantIDs)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("ids = %v, want %v (%s)", got, want, tc.why)
			}
		})
	}
}

// TestRequestFiltersNarrowOneSearch covers the filters that arrive with a
// request rather than with configuration.
func TestRequestFiltersNarrowOneSearch(t *testing.T) {
	tests := []struct {
		name    string
		filters recall.Filters
		query   string
		wantIDs []string
		why     string
	}{
		{
			name:    "project",
			filters: recall.Filters{Project: "Launch the personal site"},
			query:   "generator copy vendor",
			wantIDs: []string{"aaaa000b", "aaaa000c"},
			why:     "project comes from the CLI's project rollup, not from the bulk task shape",
		},
		{
			name:    "project match is case-insensitive",
			filters: recall.Filters{Project: "tasks"},
			query:   "vendor",
			wantIDs: []string{"aaaa0005"},
			why:     "a configured project name should not have to match the store's casing",
		},
		{
			name:    "entity matches a context",
			filters: recall.Filters{Entities: []string{"@email"}},
			query:   "vendor generator",
			wantIDs: []string{"aaaa0005"},
			why:     "contexts, tags, and project are the only entity-shaped handles a task has",
		},
		{
			name: "time window over the task's own dated boundary",
			filters: recall.Filters{
				Since: ptr(time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)),
				Until: ptr(time.Date(2026, 1, 21, 0, 0, 0, 0, time.UTC)),
			},
			query:   "vendor planning water",
			wantIDs: []string{"aaaa0005"},
			why:     "the planning and watering tasks are dated outside the window",
		},
		{
			name: "undated tasks fall outside every window",
			filters: recall.Filters{
				Since: ptr(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
			},
			query:   "generator",
			wantIDs: nil,
			why:     "a task with no date cannot be shown to fall inside a window",
		},
		{
			name:    "record type excludes this source",
			filters: recall.Filters{RecordTypes: []recall.RecordType{recall.RecordPerson}},
			query:   "vendor",
			wantIDs: nil,
			why:     "no tasks were asked for, which is an empty answer and not a failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdapter(t, recordedStore(t), nil)
			resp, err := a.Search(context.Background(), recall.SearchRequest{
				Query:    tc.query,
				Filters:  tc.filters,
				Limit:    20,
				Deadline: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if resp.Outcome != recall.SearchSuccess {
				t.Fatalf("outcome = %q, want success", resp.Outcome)
			}
			got := make([]string, 0, len(resp.Candidates))
			for _, c := range resp.Candidates {
				got = append(got, c.SourceRecordID)
			}
			slices.Sort(got)
			want := slices.Clone(tc.wantIDs)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("ids = %v, want %v (%s)", got, want, tc.why)
			}
		})
	}
}

// TestProjectRollupFailureDegradesRatherThanLies: losing the project rollup
// costs a routing field, so the search reports partial. Losing it while the
// request filtered on project means the filter could not be applied, and
// returning everything unfiltered would be a wrong answer rather than a
// smaller one.
func TestProjectRollupFailureDegradesRatherThanLies(t *testing.T) {
	broken := func(t *testing.T) *fakeCLI {
		cli := recordedStore(t)
		inner := cli.reply
		cli.reply = func(args []string) (tasks.Result, error) {
			if args[0] == "projects" {
				return tasks.Result{Stderr: []byte("task file is invalid"), ExitCode: 1}, nil
			}
			return inner(args)
		}
		return cli
	}

	t.Run("without a project filter", func(t *testing.T) {
		a := newAdapter(t, broken(t), nil)
		resp, err := search(t, a, "vendor renewal")
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if resp.Outcome != recall.SearchPartial {
			t.Errorf("outcome = %q, want partial", resp.Outcome)
		}
		if len(resp.Candidates) == 0 {
			t.Error("no candidates; the listing still answered the query")
		}
	})

	t.Run("with a project filter", func(t *testing.T) {
		a := newAdapter(t, broken(t), nil)
		resp, err := a.Search(context.Background(), recall.SearchRequest{
			Query:    "vendor renewal",
			Filters:  recall.Filters{Project: "Tasks"},
			Deadline: time.Now().Add(time.Minute),
		})
		if err == nil {
			t.Fatal("a project filter that could not be applied reported success")
		}
		if resp.Outcome == recall.SearchSuccess || resp.Outcome == recall.SearchPartial {
			t.Errorf("outcome = %q, want a failure", resp.Outcome)
		}
	})
}

// TestLimitTruncatesAndSaysSo. Truncation is a normal answer, but a caller
// cannot tell a short list from a cut one without being told.
func TestLimitTruncatesAndSaysSo(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query:    "site copy generator planning vendor water",
		Limit:    1,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("got %d candidates, want the requested 1", len(resp.Candidates))
	}
	if resp.Diagnostics["truncated"] != true {
		t.Error("diagnostics does not report the truncation")
	}
}

func find(t *testing.T, candidates []recall.Candidate, id string) recall.Candidate {
	t.Helper()
	for _, c := range candidates {
		if c.SourceRecordID == id {
			return c
		}
	}
	t.Fatalf("no candidate for %s; got %d candidates", id, len(candidates))
	return recall.Candidate{}
}

func ptr[T any](v T) *T { return &v }

// TestExcludeKeepsTheRecordWithNoContext is the case the allowlist got wrong,
// and the reason the exclude form exists at all.
//
// Configuring contexts = ["home"] to keep work out of a home profile was tried
// against the real store on 2026-07-24 and reverted: five of its records carry
// no context, "Get Mike a birthday gift" among them, and the allowlist dropped
// every one. The query then answered `coverage complete` over a task that
// exists — a false absence arriving through configuration, where nothing
// downstream can flag it.
//
// So the two forms are asserted against each other on the same record and the
// same query. If they ever agree, the exclude form has stopped being worth
// having.
func TestExcludeKeepsTheRecordWithNoContext(t *testing.T) {
	const contextless = "aaaa000f" // "Get Mike a birthday gift", contexts: []

	t.Run("allowlist drops it", func(t *testing.T) {
		a := newAdapter(t, recordedStore(t), map[string]any{
			"contexts": []any{"home"},
		})
		resp, err := search(t, a, "birthday gift")
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if ids := recordIDs(resp); slices.Contains(ids, contextless) {
			t.Fatalf("ids = %v, want the context-less record absent: this test "+
				"documents why the allowlist is the wrong form, so it failing "+
				"means the allowlist changed meaning", ids)
		}
	})

	t.Run("exclude keeps it", func(t *testing.T) {
		a := newAdapter(t, recordedStore(t), map[string]any{
			"exclude_contexts": []any{"office"},
		})
		resp, err := search(t, a, "birthday gift")
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if ids := recordIDs(resp); !slices.Contains(ids, contextless) {
			t.Errorf("ids = %v, want %s present: an exclusion naming a context "+
				"the record does not carry must exclude nothing about it",
				ids, contextless)
		}
	})

	t.Run("exclude still drops what it names", func(t *testing.T) {
		a := newAdapter(t, recordedStore(t), map[string]any{
			"exclude_contexts": []any{"@office"},
		})
		resp, err := search(t, a, "water office plants birthday gift")
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		ids := recordIDs(resp)
		if slices.Contains(ids, "aaaa0008") {
			t.Errorf("ids = %v, want the @office task dropped; the @ sigil must "+
				"normalize for exclusion exactly as it does for the allowlist", ids)
		}
		if !slices.Contains(ids, contextless) {
			t.Errorf("ids = %v, want %s kept alongside the drop", ids, contextless)
		}
	})
}

// TestExcludeAndAllowlistOnOneFacetIsRefused pins the choice not to resolve the
// combination. Intersecting them and letting the narrower win are both
// defensible, a reader of the config cannot tell which was meant, and silently
// picking one is how the allowlist defect got shipped in the first place.
func TestExcludeAndAllowlistOnOneFacetIsRefused(t *testing.T) {
	for _, facet := range []struct{ allow, exclude string }{
		{"contexts", "exclude_contexts"},
		{"tags", "exclude_tags"},
		{"states", "exclude_states"},
	} {
		t.Run(facet.allow, func(t *testing.T) {
			value := []any{"home"}
			if facet.allow == "states" {
				value = []any{"TODO"}
			}
			a := tasks.New(tasks.Options{Runner: recordedStore(t), Clock: fixedClock})
			_, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: protocol.MinVersion,
				ProtocolVersionMax: protocol.MaxVersion,
				Workdir:            t.TempDir(),
				SourceID:           "tasks",
				Settings:           map[string]any{facet.allow: value, facet.exclude: value},
			})
			if err == nil {
				t.Fatalf("%s + %s: want an error, got none", facet.allow, facet.exclude)
			}
		})
	}
}

// TestCompletedWorkRanksBelowActive covers Marcus's condition from 2026-07-28:
// finished work should not compete with live work on equal terms.
//
// It is a demotion and not a filter, and the difference is the point. "What did
// I decide about the expense report" is answered by the done task, so dropping
// it would be wrong; "what about the expense report" almost always means the
// open one, so ranking them equally is also wrong.
func TestCompletedWorkRanksBelowActive(t *testing.T) {
	a := newAdapter(t, recordedStore(t), map[string]any{"scope": "all"})
	resp, err := search(t, a, "expense report copy landing")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	ids := recordIDs(resp)
	done := slices.Index(ids, "aaaa0007") // DONE  "File the Q4 expense report"
	open := slices.Index(ids, "aaaa000c") // TODO  "Write the landing-page copy"
	if done < 0 {
		t.Fatalf("ids = %v, want the done task present: this is a demotion, not a filter", ids)
	}
	if open < 0 {
		t.Fatalf("ids = %v, want the open task present", ids)
	}
	if done < open {
		t.Errorf("done ranked %d, open ranked %d, in %v: a terminal record must "+
			"not outrank active work that matched as well", done, open, ids)
	}
}

// TestFilteredRecordsAreCounted is the home config's second stated condition
// for restoring a filter. A filter is the one way a result set shrinks without
// any query saying so, so the response has to be able to say it did.
func TestFilteredRecordsAreCounted(t *testing.T) {
	a := newAdapter(t, recordedStore(t), map[string]any{
		"exclude_contexts": []any{"computer"},
	})
	resp, err := search(t, a, "planning outline generator static birthday")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	total, ok := resp.Diagnostics["filtered_records"].(int)
	if !ok || total == 0 {
		t.Fatalf("filtered_records = %v, want a positive count", resp.Diagnostics["filtered_records"])
	}
	by, ok := resp.Diagnostics["filtered_by"].(map[string]int)
	if !ok || by["exclude_contexts"] != total {
		t.Errorf("filtered_by = %v, want %d attributed to exclude_contexts: a "+
			"count that cannot name the filter does not tell an operator which "+
			"line of config to look at", resp.Diagnostics["filtered_by"], total)
	}
}

// TestUnfilteredSearchReportsNoFilterCount keeps the diagnostic honest in the
// common case: an always-present zero would read as "a filter ran and removed
// nothing" on every request that has no filter at all.
func TestUnfilteredSearchReportsNoFilterCount(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)
	resp, err := search(t, a, "vendor renewal quote")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, present := resp.Diagnostics["filtered_records"]; present {
		t.Errorf("filtered_records present with no filter configured: %v",
			resp.Diagnostics["filtered_records"])
	}
}

// recordIDs is the source record id of each candidate, in rank order.
func recordIDs(resp recall.SearchResponse) []string {
	out := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, c.SourceRecordID)
	}
	return out
}
