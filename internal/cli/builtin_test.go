package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/recall"
)

// Every other test in this package replaces the adapters, so this one proves
// what a fake cannot: that a compiled-in adapter is registered under the name
// configuration uses, completes a real handshake, builds a real index, and
// reports health the transport can render.
//
// Only the documents adapter is configured. The Tasks adapter is compiled in
// and never constructed, so nothing here can reach the real Tasks binary or a
// personal store.
func TestSourcesOverTheBuiltInDocumentAdapter(t *testing.T) {
	corpus := t.TempDir()
	write(t, filepath.Join(corpus, "ranking.md"), `# Ranking

Cross-source fusion uses rank. Raw relevance scores from different sources are
never compared or normalized onto a shared scale.
`)

	h := newHarness(t, harnessOptions{
		userTOML: strings.ReplaceAll(builtinDocsTOML, "CORPUS", corpus),
		// Adapters left nil: the compiled-in set is what this test is about.
	})

	code, stdout, stderr := h.run("sources", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var listing cli.SourceListing
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatal(err)
	}
	got := listing.Sources[0]
	if got.AdapterID == "" {
		t.Error("no manifest came back from the handshake")
	}
	if got.Health == nil || got.Health.Status != recall.HealthHealthy {
		t.Fatalf("health = %+v, want healthy after a completed index build", got.Health)
	}
	if got.Health.IndexGeneration == "" {
		t.Error("a healthy indexed source reported no generation; a healthy index can still be stale")
	}
	if got.Health.RecordCount == 0 {
		t.Error("the corpus walk found no documents")
	}
}

// The end-to-end path a person actually uses, over the reference external
// adapter and the real wire protocol: a query returns a locator, and that
// locator is what expansion takes. If the two ever disagree, every printed
// result is a dead reference.
func TestQueryAndExpandOverAnExternalAdapter(t *testing.T) {
	corpus := filepath.Join(t.TempDir(), "events.jsonl")
	write(t, corpus, strings.Join([]string{
		`{"schema":2,"id":"e-1","event_time":"2026-03-04T05:06:07Z","title":"Fusion","text":"Cross-source fusion uses rank, never a shared scale."}`,
		`{"schema":2,"id":"e-2","event_time":"2026-03-05T05:06:07Z","title":"Gardening","text":"Tomatoes want sun."}`,
	}, "\n")+"\n")

	bin := buildStreamAdapter(t)
	h := newHarness(t, harnessOptions{
		userTOML: strings.NewReplacer("BINARY", bin, "CORPUS", corpus).Replace(streamTOML),
	})

	code, stdout, stderr := h.run("query", "--json", "--budget-ms", "20000", "fusion rank")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatalf("a stream containing the query terms produced no results\n%s", stdout)
	}
	locator := resp.Results[0].Primary.Locator.String()
	if !strings.HasPrefix(locator, "events:") {
		t.Fatalf("locator = %q, want it prefixed with the configured source name", locator)
	}

	code, evidence, stderr := h.run("expand", "--detail", "full", locator)
	if code != cli.ExitOK {
		t.Fatalf("expand %s exited %d: %s", locator, code, stderr)
	}
	if !strings.Contains(evidence, "shared scale") {
		t.Errorf("expanded evidence does not contain the record's text:\n%s", evidence)
	}
}

// buildStreamAdapter compiles the reference external adapter. It is built
// rather than faked because the point of the test is the process boundary.
func buildStreamAdapter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "recall-stream")
	build := exec.Command("go", "build", "-o", bin, "github.com/marcus/recall/cmd/recall-stream")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the reference adapter: %v\n%s", err, out)
	}
	return bin
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const builtinDocsTOML = `
[defaults]
profile = "work"
timeout_ms = 20000

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "documents"
location = "CORPUS"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs"]
max_sensitivity = "internal"
`

const streamTOML = `
[defaults]
profile = "work"
timeout_ms = 20000

[adapters.stream]
command = "BINARY"
freshness_modes = ["indexed"]

[[sources]]
source_uid = "01UIDEVENTS"
source_id = "events"
adapter = "stream"
location = "CORPUS"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["events"]
max_sensitivity = "internal"
`
