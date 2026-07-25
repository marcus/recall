package td_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// TestLiveBinary runs the real td against workspaces this test created.
//
// It is the only check that the recorded fixtures still describe td's actual
// JSON contract. Every other test in this package replays those recordings, so
// if td changes shape they would all keep passing while the adapter quietly
// broke; this one would not.
//
// The workspaces are built with td's own commands rather than by writing a
// database, because the database is td's private schema and this adapter's
// whole boundary is that it never touches one. It writes only under t.TempDir,
// so no real workspace is read or modified.
//
// It skips under -short and when the binary is absent, so a machine without td
// installed still passes the suite.
func TestLiveBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	root := liveWorkspace(t, binary, "recall",
		issueSpec{
			title:       "Adapter interface, supervision, and pooling",
			labels:      "track-adapter",
			priority:    "P1",
			description: "Deadline enforcement: cancel notification, then SIGTERM, then SIGKILL.",
			acceptance:  "- A hanging fixture adapter is killed at its deadline",
			log:         "Supervision, deadlines, pooling landed",
		},
		issueSpec{
			title:       "Lineage grouping and corroboration units",
			labels:      "track-ranking",
			priority:    "P2",
			description: "Corroboration counts units, not lineage groups.",
		},
	)

	a := newLiveAdapter(t, binary, "td-recall", root)

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
	if health.SourceWatermark == "" {
		t.Error("no watermark from a live workspace")
	}

	// Retrieval, with the typed fields intact and td's own ordering preserved.
	resp, err := search(t, a, "supervision")
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
	if !strings.HasPrefix(top.Title, "Adapter interface") {
		t.Errorf("rank 1 = %q, want the issue whose title says supervision", top.Title)
	}
	if top.Metadata["status"] != "open" || top.Metadata["priority"] != "P1" {
		t.Errorf("typed metadata did not survive the live CLI: %#v", top.Metadata)
	}
	if got, _ := top.Metadata["labels"].([]string); len(got) == 0 || got[0] != "track-adapter" {
		t.Errorf("metadata[labels] = %#v, want the label the issue was created with", top.Metadata["labels"])
	}
	if top.Metadata["workspace"] != "recall" {
		t.Errorf("metadata[workspace] = %#v, want the workspace name", top.Metadata["workspace"])
	}
	if top.EventTime == nil {
		t.Error("no event_time: td publishes created_at on every issue")
	}

	// Exact id lookup against td's real resolver, which also accepts a bare
	// suffix and a different case — the reason the id is compared again.
	id := top.SourceRecordID
	exact, err := search(t, a, "where did we land on "+id+"?")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(exact.Candidates) == 0 || !exact.Candidates[0].Exact() {
		t.Fatalf("no exact_identifier candidate at rank 1: %+v", exact.Candidates)
	}
	upper, err := search(t, a, "where did we land on "+strings.ToUpper(id)+"?")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, c := range upper.Candidates {
		if c.Exact() {
			t.Errorf("%s claims exact_identifier for an uppercase spelling", c.SourceRecordID)
		}
	}

	// Expansion reads what td's own search cannot see: the acceptance criteria
	// and the progress log.
	evidence, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  top.Locator,
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("expand %s: %v", top.Locator, err)
	}
	for _, want := range []string{"SIGKILL", "killed at its deadline", "Supervision, deadlines, pooling landed"} {
		if !strings.Contains(evidence.Content, want) {
			t.Errorf("expansion is missing %q:\n%s", want, evidence.Content)
		}
	}

	t.Logf("live td: %v ms wall for one search across %v invocations",
		resp.Diagnostics["cli_wall_ms"], resp.Diagnostics["cli_invocations"])
}

// TestLiveWorkspaceBoundary is the design point of this adapter, exercised
// against the real binary: two workspaces, one adapter, two source instances.
//
// td mints six random hex characters per workspace and guarantees uniqueness
// only within one database, so an id from one workspace is a perfectly
// plausible id in another. Everything below is what keeps those two apart.
func TestLiveWorkspaceBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)

	homeRoot := liveWorkspace(t, binary, "home", issueSpec{
		title: "Fusion across sources", priority: "P1",
		description: "Rank fusion never compares raw scores across sources.",
	})
	workRoot := liveWorkspace(t, binary, "work", issueSpec{
		title: "Fusion across sources", priority: "P1",
		description: "Rank fusion never compares raw scores across sources.",
	})

	home := newLiveAdapter(t, binary, "td-home", homeRoot)
	work := newLiveAdapter(t, binary, "td-work", workRoot)

	homeHit := only(t, home, "fusion")
	workHit := only(t, work, "fusion")

	// Same title, same text, two workspaces: the candidates must not be the
	// same record, and each has to say where it came from.
	if homeHit.Locator.Local == workHit.Locator.Local {
		t.Fatalf("both workspaces produced the locator %q", homeHit.Locator.Local)
	}
	if !strings.HasPrefix(homeHit.Locator.Local, "home/") || !strings.HasPrefix(workHit.Locator.Local, "work/") {
		t.Errorf("locators %q and %q do not name their workspaces",
			homeHit.Locator.Local, workHit.Locator.Local)
	}
	if homeHit.Metadata["workspace"] != "home" || workHit.Metadata["workspace"] != "work" {
		t.Errorf("workspace metadata = %v and %v", homeHit.Metadata["workspace"], workHit.Metadata["workspace"])
	}
	if homeHit.Metadata["workspace_root"] == workHit.Metadata["workspace_root"] {
		t.Error("both candidates claim the same workspace root")
	}

	// The boundary itself: one instance must refuse the other's locator rather
	// than answer it from its own database.
	if _, err := work.Expand(context.Background(), recall.ExpandRequest{
		Locator:  homeHit.Locator,
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	}); err == nil {
		t.Fatal("the work workspace expanded a locator belonging to the home workspace")
	}

	// And each instance answers its own, with the evidence from its own
	// database.
	evidence, err := home.Expand(context.Background(), recall.ExpandRequest{
		Locator:  homeHit.Locator,
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("expand %s: %v", homeHit.Locator, err)
	}
	if !strings.Contains(evidence.Provenance, "home") {
		t.Errorf("provenance %q does not name the workspace the evidence came from", evidence.Provenance)
	}
}

// A directory that is not a td workspace is unavailable, against the real
// binary: td exits non-zero with `database not found`, and this is where an
// adapter would be tempted to report an empty corpus instead.
func TestLiveMissingWorkspaceIsUnavailable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	a := newLiveAdapter(t, binary, "td-missing", filepath.Join(t.TempDir(), "never-initialized"))

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Fatalf("status = %q (%v), want unavailable", health.Status, health.Diagnostics)
	}
	resp, err := search(t, a, "anything")
	if err == nil {
		t.Fatal("searching a workspace that does not exist returned no error")
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %q for a workspace that does not exist", resp.Outcome)
	}
}

// issueSpec is one issue to create in a live workspace.
type issueSpec struct {
	title       string
	priority    string
	labels      string
	description string
	acceptance  string
	log         string
}

// liveWorkspace creates a td workspace under t.TempDir and fills it.
//
// The directory name is chosen by the caller because it becomes the workspace
// name: this adapter derives identity from the location, which is exactly what
// these tests are about.
func liveWorkspace(t *testing.T, binary, name string, issues ...issueSpec) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"--work-dir=" + root}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("td %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}
	run("init")
	for _, spec := range issues {
		args := []string{"create", spec.title, "--type=task"}
		if spec.priority != "" {
			args = append(args, "--priority="+spec.priority)
		}
		if spec.labels != "" {
			args = append(args, "--labels="+spec.labels)
		}
		if spec.description != "" {
			args = append(args, "--description="+spec.description)
		}
		if spec.acceptance != "" {
			args = append(args, "--acceptance="+spec.acceptance)
		}
		created := run(append(args, "--json")...)
		if spec.log != "" {
			run("log", "--issue="+issueID(t, created), spec.log)
		}
	}
	return root
}

// issueID pulls the minted id out of `td create --json`.
func issueID(t *testing.T, payload string) string {
	t.Helper()
	_, rest, found := strings.Cut(payload, `"id": "`)
	if !found {
		t.Fatalf("no id in td create output: %s", payload)
	}
	id, _, _ := strings.Cut(rest, `"`)
	return id
}

func newLiveAdapter(t *testing.T, binary, sourceID, root string) *td.Adapter {
	t.Helper()
	a := td.New(td.Options{})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           sourceID,
		Location:           root,
		Settings:           map[string]any{"binary": binary},
	}); err != nil {
		t.Fatalf("initialize %s: %v", sourceID, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// only searches and insists on exactly one candidate, which is what makes the
// two-workspace assertions about identity rather than about ordering.
func only(t *testing.T, a *td.Adapter, query string) recall.Candidate {
	t.Helper()
	resp, err := search(t, a, query)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("search %q returned %d candidates, want 1", query, len(resp.Candidates))
	}
	return resp.Candidates[0]
}

// liveBinary finds the td executable, or skips.
func liveBinary(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv("RECALL_TD_BINARY"); explicit != "" {
		return explicit
	}
	path, err := exec.LookPath(td.DefaultBinary)
	if err != nil {
		t.Skipf("skipping: %s is not on PATH (%v)", td.DefaultBinary, err)
	}
	return path
}
