package td_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/internal/recall"
)

// Ordering inside a single probe is td's, not this adapter's: td scores a
// title-prefix hit above a description hit, and that order is what comes back.
func TestSingleProbePreservesTdOrdering(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q (%v), want success", resp.Outcome, resp.Diagnostics)
	}
	// td scored the title hit 70 and the two description hits 40; the P1 task
	// precedes the P3 chore on the priority tie-break.
	if want := []string{idAdapter, idIndexing, idPoller}; !slices.Equal(ids(resp), want) {
		t.Errorf("order = %v, want %v", ids(resp), want)
	}
	for i, c := range resp.Candidates {
		if c.LocalRank != i+1 {
			t.Errorf("candidate %d has local_rank %d; rank is the only mandatory signal", i, c.LocalRank)
		}
	}
	if score := resp.Candidates[0].LocalScore; score == nil || *score != 70 {
		t.Errorf("local_score = %v, want td's own 70", score)
	}
	if got := resp.Diagnostics["query_mode"]; got != "lexical" {
		t.Errorf("query_mode = %v, want lexical", got)
	}
	// The one honest thing to say about a source whose search cannot see logs
	// or comments: say so, every time.
	if _, said := resp.Diagnostics["search_scope"]; !said {
		t.Error("no search_scope diagnostic; a short result list is unreadable without it")
	}
}

// The one ranking judgment this adapter adds. td answers one term at a time,
// so only the merge can see that an issue answered to two of the query's words
// while another answered to one — and an issue that answers more of the
// question outranks one that merely scored well on part of it.
func TestTermCoverageOutranksASingleStrongerScore(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter corroboration lineage")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := ids(resp)
	if len(got) == 0 || got[0] != idLineage {
		t.Fatalf("order = %v, want %s first: it answered to two probes, the others to one", got, idLineage)
	}
	if len(got) < 2 || got[1] != idAdapter {
		t.Errorf("order = %v, want %s second on td's own score", got, idAdapter)
	}

	// Every probe is one process. The cap is what keeps that count a function
	// of configuration rather than of how much text was pasted.
	if n := cli.countCalls("search"); n != 4 {
		t.Errorf("%d search invocations, want 4: the phrase plus three terms", n)
	}
	// The whole phrase is probed too, because a verbatim hit is the strongest
	// evidence td can produce. Probes run concurrently, so this is a set and
	// not an order.
	if q := cli.queries(); !slices.Contains(q, "adapter corroboration lineage") {
		t.Errorf("probes = %v, want the whole phrase among them", q)
	}
}

// Coverage is only a count of query terms if each probe saw its whole match
// set. max_candidates caps the OUTPUT list, and wiring it to the probes as well
// made a source configured to return one candidate also read one match per
// term, so coverage silently meant "how many probes held this issue in their
// own top max_candidates".
func TestProbeLimitIsIndependentOfMaxCandidates(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, map[string]any{"max_candidates": 1})

	resp, err := search(t, a, "adapter corroboration lineage")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Errorf("candidates = %d, want max_candidates to still cap the output list at 1", len(resp.Candidates))
	}

	listing := limits(cli, "list")
	if len(listing) != 1 {
		t.Fatalf("listing invocations = %d, want 1", len(listing))
	}
	for _, probe := range limits(cli, "search") {
		if probe == 1 {
			t.Fatalf("a probe read --limit=1: max_candidates is setting the probe limit, "+
				"so coverage counts top-1 placements rather than matching terms (probes %v)",
				limits(cli, "search"))
		}
		if probe != listing[0] {
			t.Errorf("probe --limit=%d, listing --limit=%d; both are this adapter's own bound "+
				"on how much of one workspace a search may read", probe, listing[0])
		}
	}
}

// A probe that came back holding exactly its limit did not see its whole match
// set, so every coverage count it contributed to is a floor. Reporting success
// would present an order computed on partial input as a complete one, which is
// the failure the limit change above exists to make impossible in practice and
// this report exists to catch when it happens anyway.
func TestATruncatedProbeIsReportedRatherThanRankedSilently(t *testing.T) {
	info := fixture(t, "info.json")
	listAll := fixture(t, "list_all.json")
	cli := &fakeCLI{reply: func(args []string) (td.Result, error) {
		switch args[0] {
		case "info":
			return ok(info), nil
		case "list":
			return ok(listAll), nil
		case "search":
			// Exactly what td returns when it has more to give: a full page.
			return ok(saturatedSearch(limitArg(t, args))), nil
		}
		t.Errorf("unexpected invocation: td %s", strings.Join(args, " "))
		return td.Result{}, nil
	}}
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %q, want partial: the coverage that ordered this list was counted "+
			"over part of one term's matches", resp.Outcome)
	}
	if got := resp.Diagnostics["probes_truncated"]; got != 1 {
		t.Errorf("diagnostics[probes_truncated] = %v, want 1 (%v)", got, resp.Diagnostics)
	}
	if _, said := resp.Diagnostics["probe_limit"]; !said {
		t.Error("no probe_limit diagnostic; the count of truncated probes means nothing without the bound")
	}
	if _, said := resp.Diagnostics["term_coverage"]; !said {
		t.Error("no term_coverage diagnostic; nothing else says the ranking judgment ran on partial input")
	}
}

// limits reports the `--limit=` every invocation of one subcommand carried.
func limits(f *fakeCLI, subcommand string) []int {
	var out []int
	for _, call := range f.invocations() {
		if len(call) == 0 || call[0] != subcommand {
			continue
		}
		for _, arg := range call {
			if n, ok := strings.CutPrefix(arg, "--limit="); ok {
				parsed, err := strconv.Atoi(n)
				if err != nil {
					continue
				}
				out = append(out, parsed)
			}
		}
	}
	return out
}

func limitArg(t *testing.T, args []string) int {
	t.Helper()
	for _, arg := range args {
		if n, ok := strings.CutPrefix(arg, "--limit="); ok {
			parsed, err := strconv.Atoi(n)
			if err != nil {
				t.Fatalf("unreadable --limit: %v", err)
			}
			return parsed
		}
	}
	t.Fatalf("no --limit in %v", args)
	return 0
}

// saturatedSearch renders n distinct search hits: what td returns when its
// limit, rather than the end of the match set, stopped it.
func saturatedSearch(n int) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"Issue":{"id":"td-%06x","title":"issue %d","status":"open","type":"task",`+
			`"priority":"P2","created_at":"2026-07-01T00:00:00Z","updated_at":"2026-07-01T00:00:00Z"},`+
			`"Score":40,"MatchField":"description"}`, i, i)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func TestProbeCountIsBoundedByConfiguration(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, map[string]any{"max_term_probes": 1})

	if _, err := search(t, a, "adapter lineage corroboration supervision indexing"); err != nil {
		t.Fatalf("search: %v", err)
	}
	// One term, plus the whole phrase, which is not a term and is bounded by
	// its own length limit. Five words would otherwise be five processes.
	if n := cli.countCalls("search"); n != 2 {
		t.Errorf("%d search invocations under max_term_probes=1, want 2: one term and the phrase", n)
	}
	if q := cli.queries(); slices.Contains(q, "indexing") {
		t.Errorf("probes = %v; the cap should have dropped the shortest terms", q)
	}
}

// A query too long to be a phrase costs only its terms: a sentence cannot
// appear verbatim in a title, so probing it would buy nothing for one process.
func TestALongQueryIsNotProbedWhole(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	long := "what did we decide about the adapter interface, its supervision, and pooling"
	if _, err := search(t, a, long); err != nil {
		t.Fatalf("search: %v", err)
	}
	if q := cli.queries(); slices.Contains(q, long) {
		t.Errorf("probes = %v, want no whole-query probe", q)
	}
}

// An id in the query is a direct instruction to retrieve one record. It
// partitions above everything else and carries the signal that drives the
// core's exact-match promotion.
func TestExactIdentifierPartitionsAboveLexicalHits(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := search(t, a, "where did we land on "+idLineage+" and the adapter work?")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	top := resp.Candidates[0]
	if top.SourceRecordID != idLineage {
		t.Fatalf("rank 1 = %s, want the id named in the query", top.SourceRecordID)
	}
	if !top.Exact() {
		t.Errorf("rank 1 carries %v, want exact_identifier", top.MatchSignals)
	}
	for _, c := range resp.Candidates[1:] {
		if c.Exact() {
			t.Errorf("%s claims exact_identifier without being named in the query", c.SourceRecordID)
		}
	}
	if got := resp.Diagnostics["query_mode"]; got != "exact+lexical" {
		t.Errorf("query_mode = %v, want exact+lexical", got)
	}
}

// exact_identifier is a partition in fusion, not a score bonus, so a token
// that merely looks like an id must never carry it. The false-positive half of
// this table is the half that matters.
func TestExactIdentifierOnlyAtTokenBoundaries(t *testing.T) {
	cases := []struct {
		query string
		exact bool
		why   string
	}{
		{query: idAdapter, exact: true, why: "the id alone"},
		{query: "fix " + idAdapter + ".", exact: true, why: "sentence punctuation is not part of an id"},
		{query: "(" + idAdapter + ")", exact: true, why: "brackets are not part of an id"},
		{query: strings.ToUpper(idAdapter), why: "td mints lowercase; a case variant is another spelling"},
		{query: idAdapter + "-adapter", why: "a branch name that starts with an id is not an id"},
		{query: "277316", why: "six hex characters without the prefix are not an id"},
		{query: "x" + idAdapter, why: "an id inside a longer word is a substring, not a reference"},
		{query: "issues/" + idAdapter, why: "a path segment is not a token boundary for ids"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			a := newAdapter(t, recordedWorkspace(t), nil)
			resp, err := search(t, a, tc.query)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			var claimed bool
			for _, c := range resp.Candidates {
				claimed = claimed || c.Exact()
			}
			if claimed != tc.exact {
				t.Errorf("exact_identifier = %v for %q, want %v: %s", claimed, tc.query, tc.exact, tc.why)
			}
		})
	}
}

// An exact id reaches past the instance's configured scope, because asking
// about an id is asking about that issue and not about the open list. The
// listing cannot answer it, so td is asked directly.
func TestExactIdentifierReachesOutsideTheConfiguredScope(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, map[string]any{"statuses": []any{"open"}})

	resp, err := search(t, a, idPoller)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := ids(resp); !slices.Equal(got, []string{idPoller}) {
		t.Fatalf("candidates = %v, want the closed issue the id named", got)
	}
	if !resp.Candidates[0].Exact() {
		t.Error("the record fetched by id carries no exact_identifier signal")
	}
	if n := cli.countCalls("show"); n != 1 {
		t.Errorf("%d show invocations, want 1: the listing does not hold this id", n)
	}
	if resp.Candidates[0].ConfirmedAt != nil {
		t.Error("confirmed_at is set for a record no complete listing held; a show is not a boundary")
	}
}

// An id the workspace does not hold is an answer, not a failure: td resolved
// the workspace and said no such issue.
func TestUnknownIdentifierIsAnEmptySuccess(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := search(t, a, idNotPresent)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %q, want success: the workspace answered", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("candidates = %v, want none", ids(resp))
	}
}

// A query with nothing to probe is a structured request: the filters are the
// question and the listing is the answer.
func TestStructuredQueryAnswersFromTheListing(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	resp, err := search(t, a, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := resp.Diagnostics["query_mode"]; got != "structured" {
		t.Errorf("query_mode = %v, want structured", got)
	}
	if n := cli.countCalls("search"); n != 0 {
		t.Errorf("%d search invocations for a query with no terms", n)
	}
	// Priority first, then most recently touched: the order a person scanning
	// a workspace expects, and total, so the same workspace always answers the
	// same way.
	want := []string{idIndexing, idAdapter, idEpic, idLineage, idPoller}
	if !slices.Equal(ids(resp), want) {
		t.Errorf("order = %v, want %v", ids(resp), want)
	}
	for _, c := range resp.Candidates {
		if !c.HasSignal(recall.MatchField) {
			t.Errorf("%s carries %v; a structured hit is a field match", c.SourceRecordID, c.MatchSignals)
		}
	}
}

// Every candidate says which workspace it came from, in its locator and in its
// metadata. One adapter serves many workspaces, and evidence that could not
// name its own would make provenance a property of configuration.
func TestCandidatesCarryTheWorkspaceAndTheirTypedFields(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	top := resp.Candidates[0]
	if want := "tdfix/" + idAdapter; top.Locator.Local != want {
		t.Errorf("locator local = %q, want %q", top.Locator.Local, want)
	}
	if top.SourceRecordID != idAdapter || top.CandidateID != idAdapter {
		t.Errorf("record id = %q, candidate id = %q, want both %q",
			top.SourceRecordID, top.CandidateID, idAdapter)
	}
	if top.RecordType != recall.RecordTask {
		t.Errorf("record_type = %q, want task", top.RecordType)
	}
	for key, want := range map[string]any{
		"workspace":      "tdfix",
		"workspace_root": workspaceRoot,
		"status":         "open",
		"type":           "task",
		"priority":       "P1",
		"epic":           idEpic,
		"td_match_field": "title",
		"td_score":       70,
	} {
		if got := top.Metadata[key]; got != want {
			t.Errorf("metadata[%s] = %#v, want %#v", key, got, want)
		}
	}
	if labels, _ := top.Metadata["labels"].([]string); !slices.Contains(labels, "track-adapter") {
		t.Errorf("metadata[labels] = %#v, want the issue's labels as a list", top.Metadata["labels"])
	}
	if top.EventTime == nil || top.EventTime.Year() != 2026 {
		t.Errorf("event_time = %v, want when the issue was raised", top.EventTime)
	}
	if top.ValidTo != nil {
		t.Error("valid_to is set on an open issue; the work has not stopped being live")
	}
	if top.ConfirmedAt == nil {
		t.Error("confirmed_at is unset for a record the complete listing held")
	}
	if top.Sensitivity != recall.SensitivityInternal {
		t.Errorf("sensitivity = %v, want the source floor", top.Sensitivity)
	}
	if top.SourceRevision == "" {
		t.Error("no source_revision: nothing records which workspace state answered")
	}

	// A closed issue stopped being true when it closed, and says so.
	closed := candidate(t, resp, idPoller)
	if closed.ValidTo == nil {
		t.Error("valid_to is unset on a closed issue")
	}
}

// Search is where the workspace fingerprint comes from, because search is what
// reads the listing: it needs the structured fallback and the free id lookups
// anyway, so the watermark is free there and 1.6 MB of JSON anywhere else.
// Health says it has none rather than reporting one it did not read.
func TestTheWatermarkComesFromTheReadThatProducedIt(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := search(t, a, "adapter")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.SourceWatermark == "" {
		t.Error("no watermark on a search that read the listing")
	}
	for _, c := range resp.Candidates {
		if c.SourceRevision != resp.SourceWatermark {
			t.Errorf("%s carries revision %q, the response says %q; freshness reaches an answer "+
				"through the candidate", c.SourceRecordID, c.SourceRevision, resp.SourceWatermark)
		}
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.SourceWatermark != "" {
		t.Errorf("health watermark %q from a probe that read no listing", health.SourceWatermark)
	}
}

// A workspace is a project. Routing a request to the workspace it named is the
// whole reason the boundary is preserved inside one adapter.
func TestProjectFilterRoutesToTheNamedWorkspace(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	matched, err := searchWith(t, a, recall.SearchRequest{
		Query: "adapter", Limit: 20, Filters: recall.Filters{Project: "TDFIX"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matched.Candidates) == 0 {
		t.Error("a project filter naming this workspace returned nothing")
	}

	other, err := searchWith(t, a, recall.SearchRequest{
		Query: "adapter", Limit: 20, Filters: recall.Filters{Project: "braid"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if other.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %q, want success: not being asked is not a failure", other.Outcome)
	}
	if len(other.Candidates) != 0 {
		t.Errorf("candidates = %v for another workspace's project filter", ids(other))
	}
	if got := other.Diagnostics["skipped"]; got != "project" {
		t.Errorf("diagnostics[skipped] = %v, want project", got)
	}
	if n := cli.countCalls("search"); n != 1 {
		t.Errorf("%d search invocations; a source that was not asked should spawn nothing", n)
	}
}

func TestRequestFiltersNarrowTheAnswer(t *testing.T) {
	t.Run("record type this source does not hold", func(t *testing.T) {
		cli := recordedWorkspace(t)
		a := newAdapter(t, cli, nil)
		resp, err := searchWith(t, a, recall.SearchRequest{
			Query: "adapter", Limit: 20,
			Filters: recall.Filters{RecordTypes: []recall.RecordType{recall.RecordDocument}},
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 0 {
			t.Errorf("outcome = %q with %d candidates, want an empty success", resp.Outcome, len(resp.Candidates))
		}
		if cli.countCalls("list")+cli.countCalls("search") != 0 {
			t.Error("a source holding none of the requested record types still spawned td")
		}
	})

	t.Run("entity", func(t *testing.T) {
		a := newAdapter(t, recordedWorkspace(t), nil)
		resp, err := searchWith(t, a, recall.SearchRequest{
			Query: "adapter", Limit: 20, Filters: recall.Filters{Entities: []string{"wave2"}},
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if got := ids(resp); !slices.Equal(got, []string{idPoller}) {
			t.Errorf("candidates = %v, want only the wave2 issue", got)
		}
	})

	t.Run("time window", func(t *testing.T) {
		a := newAdapter(t, recordedWorkspace(t), nil)
		future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
		resp, err := searchWith(t, a, recall.SearchRequest{
			Query: "adapter", Limit: 20, Filters: recall.Filters{Since: &future},
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(resp.Candidates) != 0 {
			t.Errorf("candidates = %v, want none raised after the window opened", ids(resp))
		}
	})
}

// Configured scope is applied by td rather than after the fact, so a limit
// applies to what the instance can see rather than to what it then discards.
func TestConfiguredScopeIsAppliedByTd(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, map[string]any{"statuses": []any{"open"}, "labels": []any{"wave1"}})

	if _, err := search(t, a, "adapter"); err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, call := range cli.invocations() {
		if call[0] != "search" && call[0] != "list" {
			continue
		}
		if !hasArg(call, "--status=open") || !hasArg(call, "--labels=wave1") {
			t.Errorf("td %v carries no configured filters; a filter applied afterwards would silently shorten the page",
				call)
		}
	}
}

func TestLimitTruncatesAndSaysSo(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	resp, err := searchWith(t, a, recall.SearchRequest{Query: "adapter", Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Candidates) != 2 {
		t.Fatalf("%d candidates, want the requested 2", len(resp.Candidates))
	}
	if resp.Diagnostics["truncated"] != true {
		t.Error("a truncated result did not report it")
	}
	if resp.Diagnostics["matched"] != 3 {
		t.Errorf("diagnostics[matched] = %v, want the 3 that matched before truncation", resp.Diagnostics["matched"])
	}
}

// The manifest declares as_of none, so the core should never send a boundary.
// Refusing again costs nothing and turns an upstream bug into a clear error
// rather than a wrong answer built from current state.
func TestAsOfIsRefusedRatherThanApproximated(t *testing.T) {
	a := newAdapter(t, recordedWorkspace(t), nil)

	boundary := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	resp, err := searchWith(t, a, recall.SearchRequest{Query: "adapter", Limit: 20, AsOf: &boundary})
	if err == nil {
		t.Fatal("a search carrying as_of was answered")
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %q for a refused boundary", resp.Outcome)
	}
	if got := resp.Diagnostics["reason"]; got != "as_of_unsupported" {
		t.Errorf("diagnostics[reason] = %v, want as_of_unsupported", got)
	}
}

// Query text reaches td only as a positional argument behind `--`, so nothing
// a user types can be read as a flag or a subcommand, and only read-only
// subcommands are ever invoked.
func TestQueryTextCannotBecomeAFlagOrASubcommand(t *testing.T) {
	cli := recordedWorkspace(t)
	a := newAdapter(t, cli, nil)

	if _, err := search(t, a, "--all delete something --labels=secret"); err != nil {
		t.Fatalf("search: %v", err)
	}
	readOnly := map[string]bool{"search": true, "list": true, "show": true, "info": true}
	for _, call := range cli.invocations() {
		if !readOnly[call[0]] {
			t.Errorf("invoked td %v, which is not one of this adapter's read-only commands", call)
		}
		if call[0] != "search" {
			continue
		}
		sep := slices.Index(call, "--")
		if sep < 0 || sep != len(call)-2 {
			t.Errorf("td %v does not put its query last behind a -- separator", call)
		}
	}
}

func candidate(t *testing.T, resp recall.SearchResponse, id string) recall.Candidate {
	t.Helper()
	for _, c := range resp.Candidates {
		if c.SourceRecordID == id {
			return c
		}
	}
	t.Fatalf("no candidate for %s in %v", id, ids(resp))
	return recall.Candidate{}
}
