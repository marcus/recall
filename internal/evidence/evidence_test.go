package evidence_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/pkg/recall"
)

func TestStripsTerminalControlSequences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ansi color", "normal \x1b[31mred\x1b[0m text", "normal red text"},
		{"cursor move", "a\x1b[2Jb", "ab"},
		{"osc title set", "a\x1b]0;pwned\x07b", "ab"},
		{"osc terminated by st", "a\x1b]8;;http://x\x1b\\b", "ab"},
		{"two character escape", "a\x1b7b", "ab"},
		{"carriage return overwrite", "visible\rhidden", "visiblehidden"},
		{"bell", "alert\a", "alert"},
		{"null", "a\x00b", "ab"},
		{"bidi override", "report\u202egnp.exe", "reportgnp.exe"},
		{"bidi isolate", "\u2066spoof\u2069", "spoof"},
		{"tabs and newlines survive", "a\tb\nc", "a\tb\nc"},
		{"plain text untouched", "ordinary text", "ordinary text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := recall.Candidate{Title: tt.in}
			got, _ := evidence.Sanitize(c, evidence.DefaultLimits())
			if got.Title != tt.want {
				t.Errorf("got %q, want %q", got.Title, tt.want)
			}
		})
	}
}

// A retrieved document must not be able to hand an operator a link that acts
// when clicked. Readability is preserved; actionability is not.
func TestNeutralizesDangerousLinkSchemes(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		blocked bool
	}{
		{"https allowed", "see https://example.com/x", "see https://example.com/x", false},
		{"http allowed", "see http://example.com", "see http://example.com", false},
		{"mailto allowed", "mail mailto:a@b.c", "mail mailto:a@b.c", false},
		{"javascript blocked", "click javascript:alert(1)", "click [blocked:javascript]alert(1)", true},
		{"data blocked", "data:text/html;base64,PHNjcmlwdD4=", "[blocked:data]text/html;base64,PHNjcmlwdD4=", true},
		{"file blocked", "open file:///etc/passwd", "open [blocked:file]///etc/passwd", true},
		{"case insensitive scheme", "JavaScript:x", "[blocked:JavaScript]x", true},
		{"unknown scheme with authority blocked", "ftp://files.example.com", "[blocked:ftp]//files.example.com", true},
		// Prose is full of scheme-shaped tokens. Mangling them would make
		// excerpts unreadable for no security gain, so a bare unknown scheme
		// with no authority is left alone.
		{"colon in prose is not a scheme", "note: this matters", "note: this matters", false},
		{"todo marker survives", "TODO: fix the ranking", "TODO: fix the ranking", false},
		{"time is not a scheme", "at 12:30 today", "at 12:30 today", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := recall.Candidate{Excerpt: tt.in}
			got, notes := evidence.Sanitize(c, evidence.DefaultLimits())
			if got.Excerpt != tt.want {
				t.Errorf("got %q, want %q", got.Excerpt, tt.want)
			}
			var reported bool
			for _, n := range notes {
				if n.Action == "neutralized_link" {
					reported = true
				}
			}
			if reported != tt.blocked {
				t.Errorf("neutralization reported = %v, want %v", reported, tt.blocked)
			}
		})
	}
}

func TestBoundsOversizedFields(t *testing.T) {
	lim := evidence.Limits{Title: 10, Excerpt: 10, MetadataValue: 10, MetadataFields: 2}
	c := recall.Candidate{
		Title:   strings.Repeat("a", 500),
		Excerpt: strings.Repeat("b", 5000),
	}
	got, notes := evidence.Sanitize(c, lim)

	if n := len([]rune(got.Title)); n > 10 {
		t.Errorf("title is %d runes, limit 10", n)
	}
	if !strings.HasSuffix(got.Title, "…") {
		t.Error("truncation should be marked, not silent")
	}
	if len(notes) < 2 {
		t.Errorf("expected truncation reported for both fields, got %v", notes)
	}
}

// Truncation must not split a multi-byte rune, which would emit invalid UTF-8
// into a terminal and a model context alike.
func TestTruncationRespectsRuneBoundaries(t *testing.T) {
	lim := evidence.DefaultLimits()
	lim.Title = 5
	c := recall.Candidate{Title: strings.Repeat("日本語", 20)}
	got, _ := evidence.Sanitize(c, lim)

	if !strings.ContainsRune(got.Title, '…') {
		t.Fatal("expected a truncation marker")
	}
	for i, r := range got.Title {
		if r == '�' {
			t.Fatalf("truncation split a rune at byte %d: %q", i, got.Title)
		}
	}
}

// An over-long metadata map must drop the same fields every run, or an
// evaluation result would vary between runs of identical input.
func TestMetadataDroppingIsDeterministic(t *testing.T) {
	lim := evidence.DefaultLimits()
	lim.MetadataFields = 3

	build := func() recall.Candidate {
		md := map[string]any{}
		for i := range 10 {
			md[fmt.Sprintf("field-%02d", i)] = i
		}
		return recall.Candidate{Metadata: md}
	}

	first, _ := evidence.Sanitize(build(), lim)
	for range 20 {
		got, _ := evidence.Sanitize(build(), lim)
		if len(got.Metadata) != 3 {
			t.Fatalf("kept %d fields, want 3", len(got.Metadata))
		}
		for k := range first.Metadata {
			if _, ok := got.Metadata[k]; !ok {
				t.Fatalf("field %q kept in one run and dropped in another", k)
			}
		}
	}
}

// Typed metadata is what keeps a task from being flattened into anonymous
// text. Non-string values carry no display risk and must survive intact.
func TestTypedMetadataSurvives(t *testing.T) {
	c := recall.Candidate{Metadata: map[string]any{
		"priority": 3,
		"done":     false,
		"state":    "in_review",
	}}
	got, _ := evidence.Sanitize(c, evidence.DefaultLimits())

	if got.Metadata["priority"] != 3 || got.Metadata["done"] != false {
		t.Errorf("typed metadata altered: %#v", got.Metadata)
	}
	if got.Metadata["state"] != "in_review" {
		t.Errorf("string metadata altered: %#v", got.Metadata)
	}
}

// The invariant: an adapter may classify a candidate more restrictively than
// its source, never less. Every combination goes through the floor.
func TestSensitivityFloorCannotBeLowered(t *testing.T) {
	levels := []recall.Sensitivity{
		recall.SensitivityPublic,
		recall.SensitivityInternal,
		recall.SensitivityConfidential,
		recall.SensitivityRestricted,
	}
	for _, floor := range levels {
		for _, claimed := range levels {
			c := recall.Candidate{Sensitivity: claimed}
			got, lowered := evidence.ApplyFloor(c, floor)

			if got.Sensitivity < floor {
				t.Errorf("floor %s with claim %s gave %s", floor, claimed, got.Sensitivity)
			}
			if wantLowered := claimed < floor; lowered != wantLowered {
				t.Errorf("floor %s claim %s: lowered reported %v, want %v",
					floor, claimed, lowered, wantLowered)
			}
			if claimed > floor && got.Sensitivity != claimed {
				t.Errorf("floor %s should not block a raise to %s, got %s",
					floor, claimed, got.Sensitivity)
			}
		}
	}
}

func TestPermitEnforcesCeiling(t *testing.T) {
	ceiling := recall.SensitivityInternal
	if !evidence.Permit(recall.Candidate{Sensitivity: recall.SensitivityPublic}, ceiling) {
		t.Error("public should pass an internal ceiling")
	}
	if !evidence.Permit(recall.Candidate{Sensitivity: recall.SensitivityInternal}, ceiling) {
		t.Error("internal should pass an internal ceiling")
	}
	if evidence.Permit(recall.Candidate{Sensitivity: recall.SensitivityConfidential}, ceiling) {
		t.Error("confidential must not pass an internal ceiling")
	}
}

func result(title, excerpt string) recall.Result {
	return recall.Result{Primary: recall.Candidate{
		Title:   title,
		Excerpt: excerpt,
		Locator: recall.Locator{SourceID: "tasks", Local: "td-1"},
	}}
}

// priced is a surface with stated prices, so a shaping test measures shaping
// rather than a serializer. A result that lost its excerpt costs the compressed
// price, which is how every real surface prices one.
type priced struct{ frame, full, compressed int }

func (p priced) Frame(resp recall.QueryResponse) int {
	if len(resp.Results) != 0 {
		panic("the frame was priced with results in it")
	}
	return p.frame
}

func (p priced) Result(rank int, r recall.Result) int {
	if rank < 1 {
		panic("a result was priced without its rank")
	}
	if r.Primary.Excerpt == "" {
		return p.compressed
	}
	return p.full
}

func shapeable(n int) recall.QueryResponse {
	results := make([]recall.Result, n)
	for i := range results {
		results[i] = result(fmt.Sprintf("result title %d", i), strings.Repeat("excerpt body ", 20))
	}
	return recall.QueryResponse{Results: results, Outcome: recall.OutcomeAnswered}
}

// Shaping runs in three phases: excerpts while they fit, then one-line entries,
// then a reported truncation.
func TestShapeDegradesThroughPhases(t *testing.T) {
	resp := shapeable(20)
	got := evidence.Shape(resp, recall.Budget{ResponseTokens: 200}, priced{frame: 20, full: 60, compressed: 10})

	if len(got.Response.Results) == 0 {
		t.Fatal("shaping dropped everything")
	}
	if got.Response.Results[0].Primary.Excerpt == "" {
		t.Error("the leading result should keep its excerpt")
	}

	sawCompressed := false
	for _, r := range got.Response.Results {
		if r.Primary.Excerpt == "" {
			sawCompressed = true
			continue
		}
		if sawCompressed {
			t.Error("an excerpt appeared after compression began; phases must not interleave")
		}
	}
	if !got.Response.Truncated || got.Response.DroppedResults == 0 {
		t.Errorf("expected truncation to be reported: %+v", got.Response)
	}
	if got.Response.DroppedResults+len(got.Response.Results) != len(resp.Results) {
		t.Errorf("dropped %d + kept %d != %d input",
			got.Response.DroppedResults, len(got.Response.Results), len(resp.Results))
	}
	if got.Tokens > 200 {
		t.Errorf("shaped output costs %d tokens, budget was 200", got.Tokens)
	}
}

// The frame is what the surface prints whatever it finds. A budget that funded
// results and left the frame unpaid would be the defect this pricing exists to
// remove: the response nobody budgeted for is the one that always renders.
func TestShapeChargesTheFrameFirst(t *testing.T) {
	resp := shapeable(20)
	cheap := evidence.Shape(resp, recall.Budget{ResponseTokens: 200}, priced{frame: 20, full: 60, compressed: 10})
	dear := evidence.Shape(resp, recall.Budget{ResponseTokens: 200}, priced{frame: 150, full: 60, compressed: 10})

	// Spend on results, not the count: a dear frame leaves less to spend, and
	// what it buys with the remainder is compression's business.
	if onResults, cheaply := dear.Tokens-150, cheap.Tokens-20; onResults >= cheaply {
		t.Errorf("a dear frame left %d tokens for results and a cheap one %d; the frame must come out of the budget",
			onResults, cheaply)
	}
	if dear.Tokens > 200 || cheap.Tokens > 200 {
		t.Errorf("cost %d and %d tokens against a budget of 200", dear.Tokens, cheap.Tokens)
	}

	// A frame alone over budget carries no results rather than pretending it
	// was free, and says how many it dropped.
	none := evidence.Shape(resp, recall.Budget{ResponseTokens: 100}, priced{frame: 500, full: 60, compressed: 10})
	if len(none.Response.Results) != 0 || !none.Response.Truncated || none.Response.DroppedResults != 20 {
		t.Errorf("a frame over budget should drop every result and report it: %+v", none.Response)
	}
}

// A frame that does not fit is summarized, not waived. What survives is the
// minimal floor: the outcome, the coverage, and every source that could not
// answer, named. Those are claims about the evidence, and dropping one to save
// tokens would make the response cheaper by making it less true.
func TestShapeSummarizesAFrameItCannotAfford(t *testing.T) {
	resp := shapeable(3)
	resp.SourceOutcomes = []recall.SourceReport{
		{SourceID: "notes", Outcome: recall.SearchSuccess},
		{SourceID: "tasks", Outcome: recall.SearchUnavailable, Reason: "unreachable"},
	}
	resp.SourceSummary = &recall.SourceSummary{
		Sources:  2,
		Outcomes: map[recall.SearchOutcome]int{recall.SearchSuccess: 1, recall.SearchUnavailable: 1},
		Degraded: []string{"tasks (unreachable)"},
	}
	resp.Plan = recall.Plan{Profile: "work", Sources: []recall.PlanSource{{SourceID: "notes"}, {SourceID: "tasks"}}}

	// A ledger nobody can afford: pricing it at 400 against a budget of 100.
	ledger := func(r recall.QueryResponse) int {
		if len(r.SourceOutcomes) > 0 {
			return 400
		}
		return 40
	}
	got := evidence.Shape(resp, recall.Budget{ResponseTokens: 100}, framed{frame: ledger, result: 20})

	if got.Response.SourceOutcomes != nil {
		t.Error("the ledger was kept in a response that could not afford it")
	}
	if got.Response.SourceSummary == nil || len(got.Response.SourceSummary.Degraded) != 1 {
		t.Fatalf("the summary standing in for the ledger lost the degraded source: %+v", got.Response.SourceSummary)
	}
	if len(got.Response.Plan.Sources) != 0 || got.Response.Plan.Profile != "work" {
		t.Errorf("the plan's source list is what a budget drops, not its header: %+v", got.Response.Plan)
	}
	want := []recall.Omission{recall.OmittedSourceOutcomes, recall.OmittedPlanSources}
	if !slices.Equal(got.Response.Omitted, want) {
		t.Errorf("omitted = %v, want %v; an omission a caller cannot see reads as a source never asked",
			got.Response.Omitted, want)
	}
	if got.Tokens > 100 {
		t.Errorf("the summarized response still costs %d tokens against a budget of 100", got.Tokens)
	}
	if len(got.Response.Results) == 0 {
		t.Error("summarizing the frame bought no room to answer at all")
	}
}

// A response whose whole frame fits keeps the ledger, and then the summary
// would be the same facts twice.
func TestShapeKeepsTheLedgerItCanAfford(t *testing.T) {
	resp := shapeable(1)
	resp.SourceOutcomes = []recall.SourceReport{{SourceID: "notes", Outcome: recall.SearchSuccess}}
	resp.SourceSummary = &recall.SourceSummary{Sources: 1}

	got := evidence.Shape(resp, recall.Budget{ResponseTokens: 5000}, evidence.StructuredCost{})
	if len(got.Response.SourceOutcomes) != 1 || got.Response.SourceSummary != nil || got.Response.Omitted != nil {
		t.Errorf("an affordable frame was summarized anyway: %+v", got.Response)
	}
}

// framed prices a frame by inspecting it, which is what makes a summarized one
// cheaper than a full one.
type framed struct {
	frame  func(recall.QueryResponse) int
	result int
}

func (f framed) Frame(resp recall.QueryResponse) int { return f.frame(resp) }

func (f framed) Result(int, recall.Result) int { return f.result }

func TestShapeIsDeterministic(t *testing.T) {
	resp := shapeable(12)
	budget := recall.Budget{ResponseTokens: 400}

	first := evidence.Shape(resp, budget, evidence.StructuredCost{})
	for range 50 {
		got := evidence.Shape(resp, budget, evidence.StructuredCost{})
		if len(got.Response.Results) != len(first.Response.Results) ||
			got.Response.DroppedResults != first.Response.DroppedResults || got.Tokens != first.Tokens {
			t.Fatalf("shaping varied between runs: %+v vs %+v", first, got)
		}
	}
}

// The core's contract, deliberately not the product's: a caller holding the
// struct pays no rendering cost, so nothing is withheld from it. Every surface
// that renders one substitutes recall.DefaultResponseTokens first.
func TestZeroBudgetMeansUnbounded(t *testing.T) {
	resp := recall.QueryResponse{Results: []recall.Result{result("a", "body"), result("b", "body")}}
	got := evidence.Shape(resp, recall.Budget{}, evidence.StructuredCost{})

	if len(got.Response.Results) != 2 || got.Response.Truncated {
		t.Errorf("an unset budget should not shape: %+v", got.Response)
	}
	if got.Tokens == 0 {
		t.Error("an unbounded response should still report what it cost")
	}
}

// A surface with no cost model of its own is priced as the serialized
// response: the largest rendering, so a projection of it cannot overrun a
// budget priced this way.
func TestStructuredCostPricesTheSerialization(t *testing.T) {
	cost := evidence.StructuredCost{}
	r := result("title", strings.Repeat("excerpt body ", 20))

	full := cost.Result(1, r)
	if want := evidence.EstimateTokens(r.Primary.Excerpt); full < want {
		t.Errorf("a result priced %d tokens, its excerpt alone is %d", full, want)
	}
	stripped := r
	stripped.Primary.Excerpt = ""
	if compressed := cost.Result(1, stripped); compressed >= full {
		t.Errorf("compressed cost %d is not below the full cost %d; compression must buy budget", compressed, full)
	}

	resp := recall.QueryResponse{Results: []recall.Result{r, r}, Outcome: recall.OutcomeAnswered}
	if frame := cost.Frame(resp); frame >= full {
		t.Errorf("the frame priced %d tokens with the results still in it; it prices the response without them", frame)
	}
}

// The locator is the one field guaranteed to be rendered — it is what a person
// copies and pastes back. A bidi override inside one makes it read as a
// different record than it names, which is precisely the attack the control
// stripping exists to stop.
func TestLocatorLocalPartIsSanitized(t *testing.T) {
	c := recall.Candidate{
		Locator: recall.Locator{
			SourceID:  "notes",
			SourceUID: "uid-notes",
			Local:     "report\u202egnp.exe\x1b[31m",
		},
		DerivedFrom: []recall.Locator{{SourceID: "tasks", Local: "td-1\x00\x07"}},
	}
	got, notes := evidence.Sanitize(c, evidence.DefaultLimits())

	if got.Locator.Local != "reportgnp.exe" {
		t.Errorf("locator local = %q, want it cleaned", got.Locator.Local)
	}
	// Cleaning must not disturb the reference structure, or it stops resolving.
	if got.Locator.SourceID != "notes" || got.Locator.SourceUID != "uid-notes" {
		t.Errorf("locator identity altered: %+v", got.Locator)
	}
	if got.DerivedFrom[0].Local != "td-1" {
		t.Errorf("derived_from local = %q, want it cleaned", got.DerivedFrom[0].Local)
	}
	var reported bool
	for _, n := range notes {
		if n.Field == "locator" {
			reported = true
		}
	}
	if !reported {
		t.Error("locator cleaning should be reported like any other field")
	}
}

func TestProvenanceFieldsAreBounded(t *testing.T) {
	lim := evidence.DefaultLimits()
	lim.Provenance = 10
	c := recall.Candidate{
		SourceRecordID: strings.Repeat("r", 100),
		CandidateID:    strings.Repeat("c", 100),
		SourceRevision: "rev\x1b[2J" + strings.Repeat("v", 100),
	}
	got, _ := evidence.Sanitize(c, lim)

	for name, v := range map[string]string{
		"source_record_id": got.SourceRecordID,
		"candidate_id":     got.CandidateID,
		"source_revision":  got.SourceRevision,
	} {
		if n := len([]rune(v)); n > 10 {
			t.Errorf("%s is %d runes, limit 10", name, n)
		}
		if strings.ContainsRune(v, 0x1b) {
			t.Errorf("%s still carries an escape: %q", name, v)
		}
	}
}

// Expansion returns the largest untrusted payload Recall handles, and the one
// most likely to be pasted into a terminal or handed to a model.
func TestExpandedEvidenceIsSanitized(t *testing.T) {
	e := recall.ExpandResponse{
		Content:        "before\x1b]0;pwned\x07after\u202espoofed",
		Provenance:     "spec.md:10-20\x00",
		SourceRevision: "abc123\x1b[31m",
	}
	got, _ := evidence.SanitizeEvidence(e, evidence.DefaultLimits())

	if got.Content != "beforeafterspoofed" {
		t.Errorf("content = %q, want control sequences removed", got.Content)
	}
	if got.Provenance != "spec.md:10-20" {
		t.Errorf("provenance = %q", got.Provenance)
	}
	if got.SourceRevision != "abc123" {
		t.Errorf("source revision = %q", got.SourceRevision)
	}
}

// A field bounded here is truncated evidence whatever the adapter reported, so
// the flag has to say so rather than leaving a caller to believe it has the
// whole record.
func TestSanitizeMarksEvidenceItTruncated(t *testing.T) {
	lim := evidence.DefaultLimits()
	lim.Content = 20
	e := recall.ExpandResponse{Content: strings.Repeat("x", 500), Truncated: false}

	got, _ := evidence.SanitizeEvidence(e, lim)
	if !got.Truncated {
		t.Error("bounding evidence must set Truncated")
	}
	if got.TruncationBoundary == "" {
		t.Error("the boundary that applied should be named")
	}
}

// An excerpt kind is a claim the core reads, so only the two it defines may
// cross the boundary. Anything else is source-controlled text arriving in a
// field every surface renders beside evidence.
func TestSanitizeDropsAnUnknownExcerptKind(t *testing.T) {
	lim := evidence.DefaultLimits()

	got, notes := evidence.Sanitize(recall.Candidate{
		Excerpt:     "text",
		ExcerptKind: recall.ExcerptKind("matched\x1b[31m"),
	}, lim)
	if got.ExcerptKind != "" {
		t.Errorf("excerpt kind = %q, want it dropped", got.ExcerptKind)
	}
	if len(notes) == 0 {
		t.Error("dropping a field must be reported")
	}

	for _, kind := range []recall.ExcerptKind{"", recall.ExcerptMatched, recall.ExcerptPreview} {
		got, _ := evidence.Sanitize(recall.Candidate{Excerpt: "text", ExcerptKind: kind}, lim)
		if got.ExcerptKind != kind {
			t.Errorf("excerpt kind %q became %q", kind, got.ExcerptKind)
		}
	}
}

// A duplicate_view suppression is the one that names a record the response
// still carries. A budget that dropped that result has to take the suppression
// with it, or the response reports a second view of something the caller was
// never handed — and a caller reading the block cannot tell that from a record
// withheld outright.
func TestShapeDropsSuppressionsForResultsItDropped(t *testing.T) {
	resp := shapeable(3)
	for i := range resp.Results {
		resp.Results[i].Explanation.LineageRoot = recall.LineageRoot(fmt.Sprintf("uid:r%d", i))
	}
	resp.Suppressed = []recall.Suppression{
		{Reason: recall.SuppressDuplicateView, Count: 1,
			LineageRoot: "other:r0", FusedInto: "uid:r0"},
		{Reason: recall.SuppressDuplicateView, Count: 1,
			LineageRoot: "other:r2", FusedInto: "uid:r2"},
		{Reason: recall.SuppressSensitivity, Count: 4},
	}

	// Room for the frame and one compressed result, and nothing else.
	got := evidence.Shape(resp, recall.Budget{ResponseTokens: 30},
		priced{frame: 20, full: 60, compressed: 10})

	if len(got.Response.Results) != 1 || got.Response.DroppedResults != 2 {
		t.Fatalf("kept %d results and dropped %d, want 1 and 2",
			len(got.Response.Results), got.Response.DroppedResults)
	}
	var reasons []string
	var roots []recall.LineageRoot
	for _, s := range got.Response.Suppressed {
		reasons = append(reasons, s.Reason)
		roots = append(roots, s.LineageRoot)
	}
	if len(got.Response.Suppressed) != 2 {
		t.Fatalf("suppressed = %v (%v), want the surviving view and the sensitivity count",
			reasons, roots)
	}
	if got.Response.Suppressed[0].LineageRoot != "other:r0" {
		t.Errorf("kept %q, want the view of the result that survived", roots[0])
	}
	if got.Response.Suppressed[1].Reason != recall.SuppressSensitivity {
		t.Errorf("reasons = %v; a record withheld outright stays reported however the response was fitted",
			reasons)
	}

	// The unshaped response is still a legitimate view of the same run.
	if len(resp.Suppressed) != 3 {
		t.Errorf("shaping edited the caller's response: %d suppressions left", len(resp.Suppressed))
	}
}

// Compression takes the members with it, so the view is no longer carried —
// but the record is, and that is what the suppression claims.
func TestShapeKeepsSuppressionsForResultsItCompressed(t *testing.T) {
	resp := shapeable(2)
	for i := range resp.Results {
		resp.Results[i].Explanation.LineageRoot = recall.LineageRoot(fmt.Sprintf("uid:r%d", i))
		resp.Results[i].Members = []recall.ClusterMember{{LineageRoot: recall.LineageRoot(fmt.Sprintf("uid:r%d", i))}}
	}
	resp.Suppressed = []recall.Suppression{
		{Reason: recall.SuppressDuplicateView, Count: 1, LineageRoot: "other:r1", FusedInto: "uid:r1"},
	}

	got := evidence.Shape(resp, recall.Budget{ResponseTokens: 45},
		priced{frame: 20, full: 60, compressed: 10})

	if len(got.Response.Results) != 2 {
		t.Fatalf("kept %d results, want both compressed rather than dropped", len(got.Response.Results))
	}
	if got.Response.Results[1].Members != nil {
		t.Fatal("the result was not compressed, so the case under test did not happen")
	}
	if len(got.Response.Suppressed) != 1 {
		t.Errorf("suppressed = %+v, want the view kept: its record is still in the answer",
			got.Response.Suppressed)
	}
}
