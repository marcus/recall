package evidence_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/recall"
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

// Shaping runs in three phases: excerpts while they fit, then one-line entries,
// then a reported truncation.
func TestShapeDegradesThroughPhases(t *testing.T) {
	results := make([]recall.Result, 20)
	for i := range results {
		results[i] = result("result title here", strings.Repeat("excerpt body ", 20))
	}

	shaper := evidence.Shaper{}
	got := shaper.Shape(results, recall.Budget{ResponseTokens: 200})

	if len(got.Results) == 0 {
		t.Fatal("shaping dropped everything")
	}
	if got.Results[0].Primary.Excerpt == "" {
		t.Error("the leading result should keep its excerpt")
	}

	sawCompressed := false
	for _, r := range got.Results {
		if r.Primary.Excerpt == "" {
			sawCompressed = true
			continue
		}
		if sawCompressed {
			t.Error("an excerpt appeared after compression began; phases must not interleave")
		}
	}
	if !got.Truncated || got.Dropped == 0 {
		t.Errorf("expected truncation to be reported: %+v", got)
	}
	if got.Dropped+len(got.Results) != len(results) {
		t.Errorf("dropped %d + kept %d != %d input", got.Dropped, len(got.Results), len(results))
	}
	if got.Tokens > 200 {
		t.Errorf("shaped output costs %d tokens, budget was 200", got.Tokens)
	}
}

func TestShapeIsDeterministic(t *testing.T) {
	results := make([]recall.Result, 12)
	for i := range results {
		results[i] = result(fmt.Sprintf("title %d", i), strings.Repeat("body ", 15))
	}
	shaper := evidence.Shaper{}

	first := shaper.Shape(results, recall.Budget{ResponseTokens: 120})
	for range 50 {
		got := shaper.Shape(results, recall.Budget{ResponseTokens: 120})
		if len(got.Results) != len(first.Results) || got.Dropped != first.Dropped || got.Tokens != first.Tokens {
			t.Fatalf("shaping varied between runs: %+v vs %+v", first, got)
		}
	}
}

func TestZeroBudgetMeansUnbounded(t *testing.T) {
	results := []recall.Result{result("a", "body"), result("b", "body")}
	got := evidence.Shaper{}.Shape(results, recall.Budget{})

	if len(got.Results) != 2 || got.Truncated {
		t.Errorf("an unset budget should not shape: %+v", got)
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
