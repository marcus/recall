package api

import (
	"strings"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

// The tool text is a projection of the structured response and may not state
// anything the structure does not. Corroboration is where the two can diverge
// silently: a cluster's members are lineage roots, and two of them can be two
// chunks of one document or two views of one record — neither of which is a
// second source agreeing. The count in the explanation is the one that knows.
func TestToolTextReportsCorroborationNotMemberCount(t *testing.T) {
	fused := recall.Result{
		Primary: recall.Candidate{Locator: recall.Locator{SourceID: "projects", Local: "p-1"}},
		Members: []recall.ClusterMember{
			{LineageRoot: "uid-a:p-1"},
			{LineageRoot: "uid-b:p-1"},
		},
		Explanation: recall.Explanation{
			SourceID:      "projects",
			Corroboration: recall.CorroborationExplanation{IndependentUnits: 1},
		},
	}
	corroborated := recall.Result{
		Primary: recall.Candidate{Locator: recall.Locator{SourceID: "tasks", Local: "td-1"}},
		Members: []recall.ClusterMember{
			{LineageRoot: "uid-a:td-1"},
			{LineageRoot: "uid-b:note.md#1"},
		},
		Explanation: recall.Explanation{
			SourceID:      "tasks",
			Corroboration: recall.CorroborationExplanation{IndependentUnits: 2},
		},
	}

	text := renderQueryText(recall.QueryResponse{
		Outcome:  recall.OutcomeAnswered,
		Coverage: recall.CoverageComplete,
		Results:  []recall.Result{fused, corroborated},
		Suppressed: []recall.Suppression{{
			Reason:      recall.SuppressDuplicateView,
			Count:       1,
			LineageRoot: "uid-b:p-1",
			FusedInto:   "uid-a:p-1",
		}},
	})

	lines := strings.Split(text, "\n")
	var fusedLine, corroboratedLine string
	for _, line := range lines {
		switch {
		case strings.Contains(line, "source=projects"):
			fusedLine = line
		case strings.Contains(line, "source=tasks"):
			corroboratedLine = line
		}
	}
	if strings.Contains(fusedLine, "corroborated_by") {
		t.Errorf("one record read twice claims corroboration: %q", fusedLine)
	}
	if !strings.Contains(corroboratedLine, "corroborated_by=2") {
		t.Errorf("two independent records do not claim it: %q", corroboratedLine)
	}
	if !strings.Contains(text, "duplicate_view") {
		t.Error("the withheld view is not reported in the tool text")
	}
}
