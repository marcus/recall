package ranking_test

import (
	"slices"
	"testing"

	"github.com/marcus/recall/internal/ranking"
)

func TestClassifyQueryDistinguishesSubjectsFromIdentifiers(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"how does clara decide what to remember", ranking.QueryClassNaturalLanguage},
		{"what is braid's daily podcast pipeline", ranking.QueryClassNaturalLanguage},
		{"summarize braid's daily podcast pipeline", ranking.QueryClassNaturalLanguage},
		{"tell me about sidecar", ranking.QueryClassNaturalLanguage},
		{"tell me about sidecar.", ranking.QueryClassNaturalLanguage},
		{"how does clara v2 remember things?", ranking.QueryClassNaturalLanguage},
		{"sidecar", ranking.QueryClassIdentifier},
		{"clara", ranking.QueryClassIdentifier},
		{"projects/recall/architecture.md", ranking.QueryClassIdentifier},
		{"What is the state of aaaa0001?", ranking.QueryClassIdentifier},
		{"what changed in td-6c98c1?", ranking.QueryClassIdentifier},
		{"What is the state of project_recall?", ranking.QueryClassIdentifier},
		{"summarize epub_to_audiobook", ranking.QueryClassIdentifier},
		{"ADR log", ranking.QueryClassNaturalLanguage},
		{"restore drill", ranking.QueryClassNaturalLanguage},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := ranking.ClassifyQuery(tc.query); got != tc.want {
				t.Errorf("ClassifyQuery(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestStableIdentifiersRejectsVersionWordsAndPreservesAdapterIDs(t *testing.T) {
	tests := []struct {
		query string
		want  []string
	}{
		{"how does clara v2 remember things?", nil},
		{"how does clara address td-6c98c1?", []string{"td-6c98c1"}},
		{"What is the state of aaaa0001?", []string{"aaaa0001"}},
		{"What is the state of project_recall?", []string{"project_recall"}},
		{"summarize epub_to_audiobook", []string{"epub_to_audiobook"}},
		{"open projects/recall/architecture.md", []string{"projects/recall/architecture.md"}},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			if got := ranking.StableIdentifiers(tc.query); !slices.Equal(got, tc.want) {
				t.Errorf("StableIdentifiers(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
