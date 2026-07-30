package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/pkg/recall"
)

// Refresh is the scheduler's log surface. A closed reason vocabulary is useful
// to machines, but it must not hide the health detail that tells an operator
// how to repair a failed source.
func TestRefreshSurfacesActionableHealthDetailInBothOutputTiers(t *testing.T) {
	m := manifest()
	m.Capabilities = append(m.Capabilities, recall.CapCheckpoint)
	detail := `source_unavailable: cannot run qmd: executable file not found in $PATH`
	docs := &fake{
		manifest: m,
		health: recall.Health{
			Status: recall.HealthUnavailable, Coverage: recall.IndexUnknown,
			Diagnostics: map[string]any{"reason": "unreachable", "detail": detail},
		},
	}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs":  docs,
			"faketasks": {manifest: manifest()},
		}),
	})

	code, stdout, _ := h.run("refresh", "--source", "docs")
	if code != cli.ExitFailed {
		t.Fatalf("exit = %d, want %d\n%s", code, cli.ExitFailed, stdout)
	}
	if !strings.Contains(stdout, "reason unhealthy") || !strings.Contains(stdout, "detail "+detail) {
		t.Fatalf("human refresh omitted actionable detail:\n%s", stdout)
	}

	code, stdout, _ = h.run("refresh", "--source", "docs", "--json")
	if code != cli.ExitFailed {
		t.Fatalf("json exit = %d, want %d\n%s", code, cli.ExitFailed, stdout)
	}
	var got recall.RefreshResponse
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("refresh JSON: %v\n%s", err, stdout)
	}
	if len(got.Sources) != 1 || got.Sources[0].DiagnosticDetail != detail ||
		got.Sources[0].Health == nil ||
		got.Sources[0].Health.Diagnostics["detail"] != detail {
		t.Fatalf("structured refresh = %+v", got)
	}
}
