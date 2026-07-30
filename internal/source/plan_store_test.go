package source

import (
	"testing"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

func TestPlanRefusesEveryInstanceReadingOneStore(t *testing.T) {
	instances := []*config.SourceInstance{
		{UID: "uid-a", ID: "td-root", Adapter: "td"},
		{UID: "uid-b", ID: "td-subdir", Adapter: "td"},
	}
	verdicts := []verdict{
		storeVerdict(instances[0], "td:abcd1234"),
		storeVerdict(instances[1], "td:abcd1234"),
	}

	refuseDuplicateStores(instances, verdicts)

	for i, got := range verdicts {
		if got.reason != ReasonStoreConflict || got.target.Instance != nil {
			t.Errorf("verdict %d = %+v, want store conflict refusal", i, got)
		}
		if got.relevanceBasis != recall.RelevanceLexicalSpan {
			t.Errorf("verdict %d relevance basis = %q, want preserved lexical_span", i, got.relevanceBasis)
		}
		if root := got.diagnostics[protocol.DiagStoreIdentity]; root != "td:abcd1234" {
			t.Errorf("verdict %d store identity = %v, want opaque conflicting identity", i, root)
		}
	}
}

func TestPlanKeepsSameBasenameSeparateStoresDistinct(t *testing.T) {
	instances := []*config.SourceInstance{
		{UID: "uid-a", ID: "work-api", Adapter: "td"},
		{UID: "uid-b", ID: "oss-api", Adapter: "td"},
	}
	verdicts := []verdict{
		storeVerdict(instances[0], "td:11111111"),
		storeVerdict(instances[1], "td:22222222"),
	}

	refuseDuplicateStores(instances, verdicts)

	for i, got := range verdicts {
		if got.reason != "" || got.target.Instance != instances[i] {
			t.Errorf("verdict %d = %+v, want eligible distinct store", i, got)
		}
	}
}

func TestPlanComparesStoreIdentityOnlyWithinOneAdapter(t *testing.T) {
	instances := []*config.SourceInstance{
		{UID: "uid-a", ID: "td", Adapter: "td"},
		{UID: "uid-b", ID: "docs", Adapter: "documents"},
	}
	verdicts := []verdict{
		storeVerdict(instances[0], "store:shared"),
		storeVerdict(instances[1], "store:shared"),
	}

	refuseDuplicateStores(instances, verdicts)

	for i, got := range verdicts {
		if got.reason != "" {
			t.Errorf("verdict %d refused across adapter boundary: %+v", i, got)
		}
	}
}

func storeVerdict(inst *config.SourceInstance, identity string) verdict {
	return verdict{
		relevanceBasis: recall.RelevanceLexicalSpan,
		target: Target{
			Instance: inst,
			Health: recall.Health{
				Status: recall.HealthHealthy,
				Diagnostics: map[string]any{
					protocol.DiagStoreIdentity: identity,
				},
			},
		},
	}
}
