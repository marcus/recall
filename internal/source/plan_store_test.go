package source

import (
	"testing"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

func TestPlanRefusesEveryInstanceReadingOneStore(t *testing.T) {
	instances := []*config.SourceInstance{
		{UID: "uid-a", ID: "td-root", Adapter: "td"},
		{UID: "uid-b", ID: "td-subdir", Adapter: "td"},
	}
	verdicts := []verdict{
		storeVerdict(instances[0], "/work/recall"),
		storeVerdict(instances[1], "/work/recall"),
	}

	refuseDuplicateStores(instances, verdicts)

	for i, got := range verdicts {
		if got.reason != ReasonStoreConflict || got.target.Instance != nil {
			t.Errorf("verdict %d = %+v, want store conflict refusal", i, got)
		}
		if root := got.diagnostics[protocol.DiagStoreIdentity]; root != "/work/recall" {
			t.Errorf("verdict %d store identity = %v, want conflicting root", i, root)
		}
	}
}

func TestPlanKeepsSameBasenameSeparateStoresDistinct(t *testing.T) {
	instances := []*config.SourceInstance{
		{UID: "uid-a", ID: "work-api", Adapter: "td"},
		{UID: "uid-b", ID: "oss-api", Adapter: "td"},
	}
	verdicts := []verdict{
		storeVerdict(instances[0], "/work/api"),
		storeVerdict(instances[1], "/oss/api"),
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
		storeVerdict(instances[0], "/work/shared"),
		storeVerdict(instances[1], "/work/shared"),
	}

	refuseDuplicateStores(instances, verdicts)

	for i, got := range verdicts {
		if got.reason != "" {
			t.Errorf("verdict %d refused across adapter boundary: %+v", i, got)
		}
	}
}

func storeVerdict(inst *config.SourceInstance, identity string) verdict {
	return verdict{target: Target{
		Instance: inst,
		Health: recall.Health{
			Status: recall.HealthHealthy,
			Diagnostics: map[string]any{
				protocol.DiagStoreIdentity: identity,
			},
		},
	}}
}
