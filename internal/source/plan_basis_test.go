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
