package recall_test

import (
	"encoding/json"
	"testing"

	"github.com/marcus/recall/internal/recall"
)

func TestSensitivityIsTotallyOrdered(t *testing.T) {
	levels := []recall.Sensitivity{
		recall.SensitivityPublic,
		recall.SensitivityInternal,
		recall.SensitivityConfidential,
		recall.SensitivityRestricted,
	}
	for i := range levels {
		for j := range levels {
			gotLE := levels[i].AtMost(levels[j])
			wantLE := i <= j
			if gotLE != wantLE {
				t.Errorf("%s.AtMost(%s) = %v, want %v", levels[i], levels[j], gotLE, wantLE)
			}
		}
	}
}

// An adapter may raise a candidate above its source floor but never lower it.
// Every combination goes through Raise, so the property is asserted over all
// of them rather than at each call site.
func TestSensitivityRaiseNeverLowers(t *testing.T) {
	levels := []recall.Sensitivity{
		recall.SensitivityPublic,
		recall.SensitivityInternal,
		recall.SensitivityConfidential,
		recall.SensitivityRestricted,
	}
	for _, floor := range levels {
		for _, claimed := range levels {
			got := floor.Raise(claimed)
			if got < floor {
				t.Errorf("floor %s raised by %s gave %s, below the floor", floor, claimed, got)
			}
			if claimed > floor && got != claimed {
				t.Errorf("floor %s raised by %s gave %s, want %s", floor, claimed, got, claimed)
			}
		}
	}
}

func TestSensitivityTextRoundTrip(t *testing.T) {
	for _, name := range []string{"public", "internal", "confidential", "restricted"} {
		var s recall.Sensitivity
		if err := s.UnmarshalText([]byte(name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.String() != name {
			t.Errorf("round trip = %q, want %q", s, name)
		}
	}
	var s recall.Sensitivity
	if err := s.UnmarshalText([]byte("secret")); err == nil {
		t.Error("unknown level should not parse")
	}
	invalid := recall.Sensitivity(99)
	if _, err := invalid.MarshalText(); err == nil {
		t.Error("invalid level should refuse to marshal")
	}
}

func TestSensitivityMarshalsAsName(t *testing.T) {
	b, err := json.Marshal(struct {
		S recall.Sensitivity `json:"s"`
	}{recall.SensitivityConfidential})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"s":"confidential"}` {
		t.Fatalf("got %s, want a level name rather than an ordinal", b)
	}
}

// Outcome and Coverage are independent fields. The spec allows abstaining with
// complete coverage and answering with degraded coverage; this asserts the
// types can express both rather than collapsing into one enum.
func TestOutcomeAndCoverageAreIndependent(t *testing.T) {
	combos := []struct {
		outcome  recall.Outcome
		coverage recall.Coverage
	}{
		{recall.OutcomeAnswered, recall.CoverageComplete},
		{recall.OutcomeAnswered, recall.CoverageDegraded},
		{recall.OutcomeAbstained, recall.CoverageComplete},
		{recall.OutcomeAbstained, recall.CoverageDegraded},
		{recall.OutcomeFailed, recall.CoverageDegraded},
	}
	for _, c := range combos {
		resp := recall.QueryResponse{Outcome: c.outcome, Coverage: c.coverage}
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.outcome, c.coverage, err)
		}
		var got recall.QueryResponse
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Outcome != c.outcome || got.Coverage != c.coverage {
			t.Errorf("round trip lost %s/%s", c.outcome, c.coverage)
		}
	}
}

// Truncation is a budget fact, not a coverage fact. A truncated response with
// complete coverage must remain representable.
func TestTruncationIsNotDegradation(t *testing.T) {
	resp := recall.QueryResponse{
		Outcome:        recall.OutcomeAnswered,
		Coverage:       recall.CoverageComplete,
		Truncated:      true,
		DroppedResults: 7,
	}
	if resp.Coverage != recall.CoverageComplete {
		t.Fatal("truncation must not imply degraded coverage")
	}
}

func TestSearchOutcomeClassification(t *testing.T) {
	tests := []struct {
		outcome  recall.SearchOutcome
		searched bool
		degrades bool
	}{
		{recall.SearchSuccess, true, false},
		{recall.SearchPartial, true, true},
		{recall.SearchUnavailable, false, true},
		{recall.SearchDenied, false, true},
		{recall.SearchFailed, false, true},
		{recall.SearchTimeout, false, true},
		{recall.SearchSkipped, false, true},
	}
	for _, tt := range tests {
		if got := tt.outcome.Searched(); got != tt.searched {
			t.Errorf("%s.Searched() = %v, want %v", tt.outcome, got, tt.searched)
		}
		if got := tt.outcome.Degrades(); got != tt.degrades {
			t.Errorf("%s.Degrades() = %v, want %v", tt.outcome, got, tt.degrades)
		}
	}
}

func TestAsOfSupportHonors(t *testing.T) {
	if recall.AsOfNone.Honors() {
		t.Error("a source declaring none must never serve an as_of request")
	}
	if !recall.AsOfFilter.Honors() || !recall.AsOfSnapshot.Honors() {
		t.Error("filter and snapshot both honor as_of")
	}
}
