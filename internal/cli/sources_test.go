package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/pkg/recall"
)

// `recall sources` is the command an operator runs before believing an answer,
// so both surfaces have to carry the same capabilities, health, and freshness
// evidence. A healthy index can still be stale, which is why the generation and
// the watermarks travel with the status rather than a single "ok".
func TestSourcesHumanAndJSONCarryTheSameFacts(t *testing.T) {
	docs := &fake{
		manifest: manifest(),
		health: recall.Health{
			Status:          recall.HealthHealthy,
			Coverage:        recall.IndexComplete,
			SourceWatermark: "w-000123",
			IndexGeneration: "gen-000042",
			IndexModel:      "lexical",
			IndexConfig:     "bm25-k1.2",
			RecordCount:     12,
		},
	}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": {manifest: manifest()}}),
	})

	code, human, _ := h.run("sources")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, human)
	}
	_, machine, _ := h.run("sources", "--json")

	var listing cli.SourceListing
	if err := json.Unmarshal([]byte(machine), &listing); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, machine)
	}
	if len(listing.Sources) != 2 {
		t.Fatalf("listed %d sources, want 2", len(listing.Sources))
	}
	got := listing.Sources[0]
	facts := map[string]string{
		"profile":          listing.Profile,
		"source id":        got.SourceID,
		"source uid":       string(got.SourceUID),
		"adapter":          got.Adapter,
		"adapter id":       got.AdapterID,
		"freshness mode":   string(got.FreshnessMode),
		"as_of support":    string(got.AsOfSupport),
		"health status":    string(got.Health.Status),
		"index generation": got.Health.IndexGeneration,
		"index model":      got.Health.IndexModel,
		"index config":     got.Health.IndexConfig,
		"watermark":        got.Health.SourceWatermark,
	}
	for what, want := range facts {
		if want == "" {
			t.Fatalf("the JSON listing carries no %s; the fixture no longer exercises it", what)
		}
		if !strings.Contains(human, want) {
			t.Errorf("human output is missing the %s %q\n--- human ---\n%s", what, want, human)
		}
	}
}

// A source that cannot answer makes the listing degraded, so a health check in
// a script does not have to parse prose.
func TestSourcesExitsDegradedWhenASourceIsDown(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: unreachableTOML})

	code, stdout, _ := h.run("sources")
	if code != cli.ExitDegraded {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitDegraded, stdout)
	}
	contains(t, stdout, "error", "the probe failure is shown, not hidden behind a status word")
}

// A disabled source, or one the ceiling denies, is listed and never contacted:
// probing it would send traffic the configuration said not to send.
func TestSourcesDoesNotProbeWhatItMayNotUse(t *testing.T) {
	vault := &fake{manifest: manifest()}
	h := newHarness(t, harnessOptions{
		userTOML: ceilingTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs": {manifest: manifest()}, "fakevault": vault,
		}),
	})

	code, stdout, _ := h.run("sources", "--json")
	if code != cli.ExitOK {
		t.Errorf("exit = %d; a denied source is the configuration working as asked", code)
	}
	var listing cli.SourceListing
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatal(err)
	}
	for _, s := range listing.Sources {
		if s.SourceID != "vault" {
			continue
		}
		if s.Probed {
			t.Error("a source above the profile ceiling was contacted")
		}
		if s.Permitted {
			t.Error("a source above the ceiling is reported as permitted")
		}
	}
}
