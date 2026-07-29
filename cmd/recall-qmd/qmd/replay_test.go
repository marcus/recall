package qmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// replayPack writes a replay directory of the shape a committed evaluation pack
// ships: recorded qmd output, a stated clock, and the corpus expansion reads.
func replayPack(t *testing.T, results string, extra ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "qmd-pack")
	corpusDir := filepath.Join(dir, qmd.ReplayCorpusDir)
	if err := os.MkdirAll(filepath.Join(corpusDir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "notes", "tooth-care.md"),
		[]byte("# Tooth care\n\nFind a dental hygienist who takes the Sample Dental plan.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The recorded reports name the corpus through the token, exactly as the
	// committed fixtures do: a recording carrying the absolute path of the machine
	// that made it would fail the collection check everywhere else.
	write("collection-show.txt", collectionText(qmd.ReplayRootToken+"/"+qmd.ReplayCorpusDir, "fixture"))
	write("status.txt", statusText(qmd.ReplayRootToken+"/"+qmd.ReplayCorpusDir, "fixture", 1, 1, 1))
	write("version.txt", "qmd 2.5.3\n")
	write("results.json", results)
	write("clock.json", `{"now": "`+fixtureClock+`"}`)

	delay := ""
	if len(extra) > 0 {
		delay = extra[0]
	}
	write(qmd.ReplayFile, `{
  "invocations": [
    {"contains": ["--version"], "stdout": "version.txt"},
    {"contains": ["collection", "show"], "stdout": "collection-show.txt"},
    {"contains": ["status"], "stdout": "status.txt"},
    {"contains": ["query"], "stdout": "results.json"`+delay+`}
  ],
  "default": {"stderr": "no recorded response", "exit_code": 1}
}`)
	return dir
}

func replayAdapter(t *testing.T, dir string) *qmd.Adapter {
	t.Helper()
	a := qmd.New(qmd.Options{})
	settings := baseSettings()
	settings["replay"] = dir
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: filepath.Join(dir, qmd.ReplayCorpusDir), Settings: settings,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// A pack is configuration, not Go injection: an adapter that could only be made
// deterministic from a test would leave a committed evaluation pack unable to
// exercise it at all.
func TestReplayPackServesASearch(t *testing.T) {
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care", 3, 3, 1, 1, semanticExplain) + "]"
	a := replayAdapter(t, replayPack(t, results))

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 1 {
		t.Fatalf("outcome = %q with %d candidates: %v",
			resp.Outcome, len(resp.Candidates), resp.Diagnostics)
	}
	if resp.Diagnostics["transport"] != "replay" {
		t.Errorf("transport = %v", resp.Diagnostics["transport"])
	}
	// A replayed run reports no wall time of its own: a timing that varied would
	// make a committed transcript and an evaluation run irreproducible.
	if resp.Diagnostics["elapsed_ms"] != int64(0) {
		t.Errorf("elapsed_ms = %v, want 0 for a replay", resp.Diagnostics["elapsed_ms"])
	}
	// The stated clock, not the recording machine's.
	if got := resp.Candidates[0].ObservedAt; got == nil || got.Format(time.RFC3339) != fixtureClock {
		t.Errorf("observed_at = %v, want the fixture's clock", got)
	}
	// Evidence still comes from the pack's own corpus.
	got, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "qmd", Local: "notes/tooth-care.md#L3-L3"},
		Detail:   recall.DetailFull,
		Deadline: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Content, "dental hygienist") {
		t.Errorf("expanded evidence = %q", got.Content)
	}
}

// The recorded collection path is checked against the configured location like
// any other. A pack whose recording names another tree is refused, which is what
// makes the check testable without a machine path in the fixture.
func TestReplayVerifiesTheRecordedCollectionPath(t *testing.T) {
	dir := replayPack(t, "[]")
	if err := os.WriteFile(filepath.Join(dir, "collection-show.txt"),
		[]byte(collectionText(qmd.ReplayRootToken+"/other-corpus", "fixture")), 0o644); err != nil {
		t.Fatal(err)
	}
	a := replayAdapter(t, dir)
	resp, err := searchOnce(t, a, "dental")
	if outcome, _ := classify(t, resp, err); outcome != recall.SearchUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
}

// delay_ms is why a fixture can test cancellation at all: a recording that
// always answered immediately would leave the in-flight case testable only
// against the real binary, which is the thing a fixture exists to avoid needing.
func TestReplayDelayIsInterruptible(t *testing.T) {
	a := replayAdapter(t, replayPack(t, "[]", `, "delay_ms": 30000`))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := a.Search(ctx, recall.SearchRequest{
		Query: "dental", Limit: 10, Deadline: time.Now().Add(time.Minute),
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the recorded delay was waited out: %s", elapsed)
	}
	if outcome, _ := classify(t, resp, err); outcome != recall.SearchTimeout {
		t.Fatalf("outcome = %q, want timeout", outcome)
	}
}

// A request deadline shorter than the recorded delay expires, and the source
// reports a timeout rather than a lexical answer it did not produce.
func TestReplayDelayHonorsTheRequestDeadline(t *testing.T) {
	a := replayAdapter(t, replayPack(t, "[]", `, "delay_ms": 30000`))
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental", Limit: 10, Deadline: time.Now().Add(50 * time.Millisecond),
	})
	if outcome, reason := classify(t, resp, err); outcome != recall.SearchTimeout ||
		!strings.HasPrefix(reason, "deadline_exceeded") {
		t.Fatalf("outcome = %q reason = %q", outcome, reason)
	}
}

// An unrecorded invocation is a gap in the fixture. Saying so is more useful
// than an empty result the adapter would report as "no matches".
func TestReplayGapIsUnavailable(t *testing.T) {
	dir := replayPack(t, "[]")
	if err := os.WriteFile(filepath.Join(dir, qmd.ReplayFile), []byte(`{
  "invocations": [{"contains": ["collection", "show"], "stdout": "collection-show.txt"}]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	a := replayAdapter(t, dir)
	resp, err := searchOnce(t, a, "dental")
	if outcome, _ := classify(t, resp, err); outcome != recall.SearchUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
}

func TestReplayRejectsAnUnusableDirectory(t *testing.T) {
	if _, err := qmd.NewReplayRunner(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing replay directory was accepted")
	}
	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, qmd.ReplayFile),
		[]byte(`{"invocations": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := qmd.NewReplayRunner(empty); err == nil {
		t.Fatal("a recording with no rules and no default was accepted")
	}
}

// A recorded stdout path may not leave the replay directory: a fixture naming
// ../../etc would be reading something the pack does not ship.
func TestReplayStdoutStaysInsideThePack(t *testing.T) {
	dir := replayPack(t, "[]")
	if err := os.WriteFile(filepath.Join(dir, qmd.ReplayFile), []byte(`{
  "invocations": [{"contains": ["collection", "show"], "stdout": "../escape.txt"}],
  "default": {"exit_code": 1}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, err := qmd.NewReplayRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), "collection", "show", "fixture"); err == nil {
		t.Fatal("a fixture read outside the replay directory")
	}
}

// A refresh whose recording has no maintenance output cannot move the index
// forward, and it says so through health rather than through an error.
func TestReplayRefreshWithoutRecordedMaintenanceDegrades(t *testing.T) {
	a := replayAdapter(t, replayPack(t, "[]"))
	health, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil {
		t.Fatalf("a failed build must not error: %v", err)
	}
	if health.Status == recall.HealthHealthy {
		t.Fatal("a refresh that did nothing reported healthy")
	}
	if _, ok := health.Diagnostics["last_refresh_error"]; !ok {
		t.Fatalf("diagnostics do not name the failure: %v", health.Diagnostics)
	}
}
