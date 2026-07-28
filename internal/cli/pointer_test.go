package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// pointerJSON is what `recall query --json` emits, declared here rather than
// imported from the producer.
//
// A test that unmarshaled into the type query.go marshals from would pass on
// any shape both agreed to, including a renamed field no consumer expected.
// This is the wire contract stated independently, so a change to it has to be
// made twice on purpose.
type pointerJSON struct {
	Tier    string `json:"tier"`
	Results []struct {
		Rank          int    `json:"rank"`
		Locator       string `json:"locator"`
		SourceID      string `json:"source_id"`
		RecordType    string `json:"record_type"`
		Title         string `json:"title"`
		Excerpt       string `json:"excerpt"`
		ExcerptKind   string `json:"excerpt_kind"`
		Exact         bool   `json:"exact"`
		Corroboration int    `json:"corroboration"`
	} `json:"results"`
	SourceSummary *struct {
		Sources  int            `json:"sources"`
		Outcomes map[string]int `json:"outcomes"`
		Degraded []string       `json:"degraded"`
	} `json:"source_summary"`
	Suppressed     []json.RawMessage `json:"suppressed"`
	Omitted        []string          `json:"omitted"`
	Outcome        string            `json:"outcome"`
	Coverage       string            `json:"coverage"`
	Truncated      bool              `json:"truncated"`
	DroppedResults int               `json:"dropped_results"`
}

func parsePointerJSON(t *testing.T, out string) pointerJSON {
	t.Helper()
	var resp pointerJSON
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out)
	}
	if resp.Tier != "pointer" {
		t.Errorf("tier = %q, want \"pointer\": the shape has to name itself", resp.Tier)
	}
	return resp
}

// pointerHarness is one healthy source and one that cannot answer, so every
// test below sees both a result and a coverage claim.
func pointerHarness(t *testing.T) *harness {
	t.Helper()
	docs := &fake{manifest: manifest(), candidates: []recall.Candidate{candidate("a.md", 1)}}
	tasks := &fake{manifest: manifest(), searchErr: protocol.ErrSourceUnavailable}
	return newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
	})
}

// --json is the pointer tier. What it carries is what a caller needs in order
// to choose a locator to expand; what it drops is the diagnostic tier and
// nothing else.
func TestJSONDefaultsToThePointerTier(t *testing.T) {
	h := pointerHarness(t)
	_, out, _ := h.run("query", "--json", "ranking")

	resp := parsePointerJSON(t, out)
	if len(resp.Results) == 0 {
		t.Fatalf("no results to project\n%s", out)
	}
	r := resp.Results[0]
	if r.Rank != 1 {
		t.Errorf("rank = %d, want 1: a projected result carries its rank as a field", r.Rank)
	}
	if r.Locator == "" || r.Title == "" || r.Excerpt == "" {
		t.Errorf("a pointer needs a locator, a title, and an excerpt: %+v", r)
	}
	if r.SourceID == "" || r.RecordType == "" {
		t.Errorf("a machine caller routes on source and record type without "+
			"splitting a locator: %+v", r)
	}

	// The diagnostic tier, by the names it would serialize under. Substring
	// checks over the whole document catch them wherever they are nested.
	for _, key := range []string{
		`"plan"`, `"source_outcomes"`, `"members"`, `"explanation"`,
		`"score"`, `"candidate_id"`, `"local_rank"`, `"diagnostics"`,
		`"content_fingerprint"`, `"source_revision"`,
	} {
		if strings.Contains(out, key) {
			t.Errorf("%s is the diagnostic tier and --json alone must not carry it:\n%s", key, out)
		}
	}
}

// The four exempt facts. Their absence would itself be a claim, so no tier and
// no encoding may drop them — this is the same rule the human surface follows.
func TestThePointerTierStillStatesWhatTheResponseClaims(t *testing.T) {
	h := pointerHarness(t)
	code, out, _ := h.run("query", "--json", "ranking")

	if code != cli.ExitDegraded {
		t.Errorf("exit = %d, want %d: projecting the output cannot change what the run claims",
			code, cli.ExitDegraded)
	}
	resp := parsePointerJSON(t, out)
	if resp.Outcome == "" || resp.Coverage == "" {
		t.Errorf("outcome and coverage are what the answer claims: %+v", resp)
	}
	if resp.Coverage != string(recall.CoverageDegraded) {
		t.Errorf("coverage = %q, want %q", resp.Coverage, recall.CoverageDegraded)
	}
	if resp.SourceSummary == nil {
		t.Fatalf("the pointer tier drops the ledger, never the summary standing in for it:\n%s", out)
	}
	if len(resp.SourceSummary.Degraded) != 1 {
		t.Errorf("degraded = %v; a source that could not answer is named on every surface",
			resp.SourceSummary.Degraded)
	}
	if resp.SourceSummary.Sources != 2 {
		t.Errorf("sources = %d, want 2: the summary reports how many were asked, "+
			"not how many answered", resp.SourceSummary.Sources)
	}
}

// --json --explain is what --json was: every field, nothing projected. It is
// the migration path, so it has to stay exactly that.
func TestJSONExplainIsTheCompleteSerialization(t *testing.T) {
	h := pointerHarness(t)
	_, out, _ := h.run("query", "--json", "--explain", "ranking")

	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("--json --explain is not valid JSON: %v\n%s", err, out)
	}
	if len(resp.Results) == 0 {
		t.Fatalf("no results\n%s", out)
	}
	if resp.Results[0].Primary.Locator.String() == "" {
		t.Error("the complete serialization still nests the candidate under primary")
	}
	if resp.Plan.Profile == "" {
		t.Error("the plan is part of the complete serialization")
	}
	if len(resp.SourceOutcomes) != 2 {
		t.Errorf("source_outcomes = %d reports, want 2: the complete serialization "+
			"carries the whole ledger", len(resp.SourceOutcomes))
	}
	if strings.Contains(out, `"tier"`) {
		t.Error("the complete form gains no field: a consumer of the old --json " +
			"must see byte-for-byte what it saw before")
	}
}

// The projection is the point: it has to be dramatically cheaper, and cheaper
// in the part that does not grow with the answer. A twenty-result response and
// a one-result response used to serialize to nearly the same size, because the
// frame was the cost.
func TestThePointerTierIsCheaperThanTheCompleteOne(t *testing.T) {
	h := pointerHarness(t)
	_, pointer, _ := h.run("query", "--json", "ranking")
	_, complete, _ := h.run("query", "--json", "--explain", "ranking")

	if len(pointer) >= len(complete)/2 {
		t.Errorf("pointer tier is %d bytes against %d complete; the projection "+
			"is not buying anything", len(pointer), len(complete))
	}
}

// Both pointer tiers describe the same results in the same order. The
// encodings differ; the facts do not.
func TestBothPointerTiersCarryTheSameResults(t *testing.T) {
	h := pointerHarness(t)
	_, human, _ := h.run("query", "ranking")
	_, machine, _ := h.run("query", "--json", "ranking")

	resp := parsePointerJSON(t, machine)
	if len(resp.Results) == 0 {
		t.Fatalf("no results\n%s", machine)
	}
	for _, r := range resp.Results {
		contains(t, human, r.Locator, "a locator the machine tier returned")
		contains(t, human, r.Title, "a title the machine tier returned")
	}
	// The coverage claim reaches both, in each one's own vocabulary.
	contains(t, human, "degraded coverage:", "the human tier names a source that could not answer")
	if resp.SourceSummary == nil || len(resp.SourceSummary.Degraded) == 0 {
		t.Error("the machine tier names it too, in source_summary.degraded")
	}
}
