package td_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
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
	if health.SourceWatermark != "" {
		t.Errorf("watermark %q from a probe that read no listing", health.SourceWatermark)
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
	if homeHit.Metadata["workspace_store"] == workHit.Metadata["workspace_store"] {
		t.Error("both candidates claim the same opaque workspace store")
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

// TestLiveOneDatabaseTwoInstances is the defect this adapter shipped with:
// two configured locations that are one td database.
//
// td resolves its database by walking UPWARD from the directory it is given, so
// a repository and any subdirectory of it reach the same SQLite file. Identity
// taken from the configured path made those two instances `recall` and `docs`,
// and the core — which groups lineage on source_uid plus source_record_id — then
// counted one issue, in one file, as two independent pieces of evidence and
// applied the corroboration bonus for it. `recall doctor` reported ok.
//
// The repository is real git here, deliberately. Without it td finds no marker
// above the subdirectory and simply reports the workspace missing, so a test on
// a bare temp directory would pass while the live configuration stayed broken.
func TestLiveOneDatabaseTwoInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	root := liveWorkspace(t, binary, "recall", issueSpec{
		title: "Workspace identity", priority: "P1",
		description: "Identity comes from the database td opened.",
	})
	gitInit(t, root)
	sub := filepath.Join(root, "docs")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	whole := newLiveAdapter(t, binary, "td-a", root)
	inner := newLiveAdapter(t, binary, "td-sub", sub)

	wholeHealth := health(t, whole)
	innerHealth := health(t, inner)
	for _, h := range []recall.Health{wholeHealth, innerHealth} {
		if h.Status != recall.HealthHealthy {
			t.Fatalf("status = %q (%v), want healthy", h.Status, h.Diagnostics)
		}
		if got := h.Diagnostics["workspace"]; got != "recall" {
			t.Errorf("diagnostics[workspace] = %v, want the workspace td opened, not the configured directory", got)
		}
	}

	// The check `recall doctor` runs: one store, named identically by both, so
	// two instances over it are detectable before a query rather than after a
	// ranking has already been distorted.
	if a, b := wholeHealth.Diagnostics[protocol.DiagStoreIdentity], innerHealth.Diagnostics[protocol.DiagStoreIdentity]; a != b {
		t.Errorf("store identities %v and %v differ for one database", a, b)
	}

	// And the evidence itself collapses: one issue read twice is one
	// observation, so the corroboration bonus cannot be collected from it.
	wholeHit := only(t, whole, "identity")
	innerHit := only(t, inner, "identity")
	if wholeHit.ContentFingerprint == "" {
		t.Fatal("no content fingerprint: nothing would collapse the duplicate")
	}
	if wholeHit.ContentFingerprint != innerHit.ContentFingerprint {
		t.Errorf("fingerprints %q and %q differ for one issue in one database",
			wholeHit.ContentFingerprint, innerHit.ContentFingerprint)
	}
	if wholeHit.Locator.Local != innerHit.Locator.Local {
		t.Errorf("locators %q and %q differ for one issue in one database",
			wholeHit.Locator.Local, innerHit.Locator.Local)
	}

	// The false refusal, which was the same bug reached from the other side:
	// the instance configured at the repository root used to reject a locator
	// naming `docs`, for an issue held in the database it had itself opened.
	if _, err := whole.Expand(context.Background(), recall.ExpandRequest{
		Locator:  innerHit.Locator,
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	}); err != nil {
		t.Errorf("the root instance refused a locator from the database it opened: %v", err)
	}
}

// TestLiveSameBaseNameTwoDatabases is the case the duplicate check must NOT
// flag: two genuinely separate workspaces whose directories share a name.
//
// It is the counterpart to the test above, and it is what stops the fix from
// being "call every pair of td sources a duplicate". They share an identity
// string and share nothing else, so the store they name has to be what tells
// them apart.
func TestLiveSameBaseNameTwoDatabases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	work := liveWorkspace(t, binary, "api", issueSpec{
		title: "Rate limiting", priority: "P1", description: "Token bucket per tenant.",
	})
	oss := liveWorkspace(t, binary, "api", issueSpec{
		title: "Rate limiting", priority: "P1", description: "Token bucket per tenant.",
	})
	if work == oss {
		t.Fatal("both workspaces landed in one directory")
	}

	workHealth := health(t, newLiveAdapter(t, binary, "td-work-api", work))
	ossHealth := health(t, newLiveAdapter(t, binary, "td-oss-api", oss))

	if workHealth.Diagnostics["workspace"] != "api" || ossHealth.Diagnostics["workspace"] != "api" {
		t.Fatalf("workspaces %v and %v, want both named api",
			workHealth.Diagnostics["workspace"], ossHealth.Diagnostics["workspace"])
	}
	if a, b := workHealth.Diagnostics[protocol.DiagStoreIdentity], ossHealth.Diagnostics[protocol.DiagStoreIdentity]; a == b {
		t.Errorf("two separate databases both claim the store %v; the duplicate check would refuse a sound configuration", a)
	}
}

func TestLiveLocalRedirectWinsBeforeSameBasenameGitRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	gitRoot := liveWorkspace(t, binary, "api", issueSpec{
		title: "Git root issue", priority: "P2",
	})
	redirectedRoot := liveWorkspace(t, binary, "api", issueSpec{
		title: "Redirected database issue", priority: "P1",
	})
	gitInit(t, gitRoot)
	local := filepath.Join(gitRoot, "nested")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, ".td-root"), []byte(redirectedRoot+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	redirected := newLiveAdapter(t, binary, "td-redirected", local)
	direct := newLiveAdapter(t, binary, "td-direct", redirectedRoot)
	git := newLiveAdapter(t, binary, "td-git", gitRoot)

	redirectedHealth := health(t, redirected)
	directHealth := health(t, direct)
	gitHealth := health(t, git)
	if got, want := redirectedHealth.Diagnostics[protocol.DiagStoreIdentity], directHealth.Diagnostics[protocol.DiagStoreIdentity]; got != want {
		t.Fatalf("redirected store %v != direct store %v", got, want)
	}
	if got, other := redirectedHealth.Diagnostics[protocol.DiagStoreIdentity], gitHealth.Diagnostics[protocol.DiagStoreIdentity]; got == other {
		t.Fatalf("redirected and git-root databases share identity %v", got)
	}
	hit := only(t, redirected, "redirected")
	if !strings.HasPrefix(hit.Title, "Redirected") {
		t.Errorf("redirected source returned wrong database evidence: %q", hit.Title)
	}
}

// A `workspace` setting that names something other than the workspace td
// resolves to is refused, rather than renaming that database for every locator
// this source emits. This is the false ACCEPT from the report: a source
// pointing at clara-home, configured `workspace = "recall"`, answered
// `td:recall/td-224186` out of the wrong database entirely.
func TestLiveAssertedWorkspaceMustMatchTheDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short excludes tests that spawn the real td CLI")
	}
	binary := liveBinary(t)
	root := liveWorkspace(t, binary, "clara-home", issueSpec{title: "Anything", priority: "P3"})

	a := td.New(td.Options{})
	_, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "td-mislabelled",
		Location:           root,
		Settings:           map[string]any{"binary": binary, "workspace": "recall"},
	})
	t.Cleanup(func() { _ = a.Close() })
	if err != nil {
		t.Fatalf("initialize should defer identity until td opens the database: %v", err)
	}
	mismatch := health(t, a)
	if mismatch.Usable() {
		t.Fatalf("a workspace name naming another database is usable: %v", mismatch.Diagnostics)
	}
	detail, _ := mismatch.Diagnostics["identity"].(string)
	if !strings.Contains(detail, "clara-home") || !strings.Contains(detail, "recall") {
		t.Errorf("identity diagnostic does not name both asserted and opened workspaces: %v", mismatch.Diagnostics)
	}

	// The same setting, naming what td actually opened, is fine — and is
	// recorded as an identity that was asserted as well as observed.
	agreed := newLiveAdapterWith(t, binary, "td-clara-home", root, map[string]any{"workspace": "clara-home"})
	if got := health(t, agreed).Diagnostics["workspace_asserted"]; got != true {
		t.Errorf("diagnostics[workspace_asserted] = %v, want true", got)
	}
}

// gitInit makes dir a real repository, which is what lets td climb out of a
// subdirectory of it the way it does in a real checkout.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("skipping: git init failed (%v): %s", err, out)
	}
}

func health(t *testing.T, a *td.Adapter) recall.Health {
	t.Helper()
	h, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	return h
}

func newLiveAdapterWith(t *testing.T, binary, sourceID, root string, settings map[string]any) *td.Adapter {
	t.Helper()
	settings["binary"] = binary
	a := td.New(td.Options{})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           sourceID,
		Location:           root,
		Settings:           settings,
	}); err != nil {
		t.Fatalf("initialize %s: %v", sourceID, err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}
