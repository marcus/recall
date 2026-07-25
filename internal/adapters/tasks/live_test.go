package tasks_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// liveStore is a complete, valid v2 store written directly to disk.
//
// It is written rather than captured with `tasks capture` because this adapter
// is read-only and its test suite has no business mutating anything. The CLI
// still validates it: an invalid file would fail `check` and this test with
// it.
const liveStore = `{"type":"meta","version":2}
{"type":"section","id":"bbbb0001","title":"Inbox"}
{"type":"task","id":"bbbb0002","parent":"bbbb0001","state":"TODO","priority":"A","title":"Renew the domain registration","tags":["@computer"],"deadline":"2026-03-01","body":"Registrar sends a reminder first."}
{"type":"section","id":"bbbb0003","title":"Projects"}
{"type":"section","id":"bbbb0004","parent":"bbbb0003","title":"Move the blog"}
{"type":"task","id":"bbbb0005","parent":"bbbb0004","state":"NEXT","title":"Export the old posts","tags":["@computer"]}
`

// TestLiveBinary runs the real Tasks CLI against a store this test wrote.
//
// It is the only check that the recorded fixtures still describe the CLI's
// actual JSON contract. Every other test in this package replays those
// recordings, so if the CLI changes shape they would all keep passing while
// the adapter quietly broke; this one would not.
//
// It skips under -short and when the binary is absent, so a machine without
// Tasks installed still passes the suite.
func TestLiveBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real Tasks CLI")
	}
	binary := liveBinary(t)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "tasks.jsonl"), liveStore)
	write(t, filepath.Join(dir, "archive.jsonl"), `{"type":"meta","version":2}`+"\n")

	a := tasks.New(tasks.Options{})
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
		Location:           dir,
		Settings:           map[string]any{"binary": binary},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if manifest.AsOfSupport != recall.AsOfNone {
		t.Errorf("as_of_support = %q, want none", manifest.AsOfSupport)
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Fatalf("status = %q (%v), want healthy", health.Status, health.Diagnostics)
	}
	if health.RecordCount != 2 {
		t.Errorf("record_count = %d, want 2", health.RecordCount)
	}

	// Lexical retrieval, with the typed fields intact.
	resp, err := search(t, a, "renew the domain registration")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q (%v), want success", resp.Outcome, resp.Diagnostics)
	}
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidates from the live CLI")
	}
	top := resp.Candidates[0]
	if top.SourceRecordID != "bbbb0002" {
		t.Errorf("rank 1 = %s, want bbbb0002", top.SourceRecordID)
	}
	if top.Metadata["state"] != "TODO" || top.Metadata["priority"] != "A" {
		t.Errorf("typed metadata did not survive the live CLI: %#v", top.Metadata)
	}
	if top.Metadata["deadline"] != "2026-03-01" {
		t.Errorf("metadata[deadline] = %#v, want 2026-03-01", top.Metadata["deadline"])
	}
	// The CLI's project rollup covers open tasks filed under a project or
	// area, and excludes Inbox. This task is in the Inbox, so search-time
	// metadata carries no project — asserting that keeps the documented gap
	// from quietly becoming a wrong value.
	if _, present := top.Metadata["project"]; present {
		t.Errorf("metadata[project] = %#v for an Inbox task; the rollup excludes Inbox",
			top.Metadata["project"])
	}

	// Exact id lookup against the real resolver, which also accepts title
	// substrings and case variants — the reason the id is re-checked.
	exact, err := search(t, a, "status of bbbb0005?")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(exact.Candidates) == 0 || !exact.Candidates[0].Exact() {
		t.Fatalf("no exact_identifier candidate at rank 1: %+v", exact.Candidates)
	}
	if exact.Candidates[0].SourceRecordID != "bbbb0005" {
		t.Errorf("rank 1 = %s, want bbbb0005", exact.Candidates[0].SourceRecordID)
	}
	if got := exact.Candidates[0].Metadata["project"]; got != "Move the blog" {
		t.Errorf("metadata[project] = %#v, want the project this task is filed under", got)
	}

	// The case variant must not: it is the same record to the CLI's resolver
	// and a different token to this adapter.
	variant, err := search(t, a, "status of BBBB0005?")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, c := range variant.Candidates {
		if c.Exact() {
			t.Errorf("%s claims exact_identifier for an uppercase spelling", c.SourceRecordID)
		}
	}

	// Expansion reads the note the bulk listing does not carry.
	evidence, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "tasks", Local: "bbbb0002"},
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if want := "Registrar sends a reminder first."; !strings.Contains(evidence.Content, want) {
		t.Errorf("expansion = %q, want it to contain %q", evidence.Content, want)
	}

	t.Logf("live CLI: %v wall for one search across %v invocations",
		resp.Diagnostics["cli_wall_ms"], resp.Diagnostics["cli_invocations"])
}

// liveBinary finds the Tasks executable, or skips.
func liveBinary(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv("RECALL_TASKS_BINARY"); explicit != "" {
		return explicit
	}
	path, err := exec.LookPath(tasks.DefaultBinary)
	if err != nil {
		t.Skipf("skipping: %s is not on PATH (%v)", tasks.DefaultBinary, err)
	}
	return path
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
