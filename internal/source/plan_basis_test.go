package source

import (
	"testing"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/recall"
)

func TestAsPlanCarriesTheManifestRelevanceBasis(t *testing.T) {
	p := Plan{Targets: []Target{{
		Instance: &config.SourceInstance{UID: "uid-vector", ID: "semantic"},
		Manifest: recall.Manifest{RelevanceBasis: recall.RelevanceVectorSimilarity},
		Deadline: time.Now().Add(time.Second),
	}}}

	got := p.AsPlan(recall.FusionRules{})
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(got.Sources))
	}
	if got.Sources[0].RelevanceBasis != recall.RelevanceVectorSimilarity {
		t.Errorf("relevance_basis = %q, want vector_similarity", got.Sources[0].RelevanceBasis)
	}
}

func TestAsPlanCarriesKnownBasisForPostHandshakeExclusionOnly(t *testing.T) {
	p := Plan{
		Excluded: []recall.SourceReport{
			{SourceUID: "uid-known", SourceID: "known", Reason: ReasonAsOfUnsupported},
			{SourceUID: "uid-static", SourceID: "static", Reason: ReasonSensitivity},
		},
		ExcludedRelevanceBases: map[recall.SourceUID]recall.RelevanceBasis{
			"uid-known": recall.RelevanceVectorSimilarity,
		},
	}

	got := p.AsPlan(recall.FusionRules{})
	if basis := got.Sources[0].RelevanceBasis; basis != recall.RelevanceVectorSimilarity {
		t.Errorf("post-handshake basis = %q, want vector_similarity", basis)
	}
	if basis := got.Sources[1].RelevanceBasis; basis != "" {
		t.Errorf("pre-handshake basis = %q, want empty", basis)
	}
}
