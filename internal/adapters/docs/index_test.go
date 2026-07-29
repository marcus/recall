package docs_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// TestFirstHandshakePublishesAGeneration. A source that answered "unavailable"
// until something else happened to build its index would be indistinguishable
// from a broken one, so the first handshake indexes.
func TestFirstHandshakePublishesAGeneration(t *testing.T) {
	t.Parallel()
	a, workdir := newAdapter(t, cleanCorpus(t), nil)

	h := health(t, a)
	if h.Status != recall.HealthHealthy {
		t.Errorf("status = %q, want healthy over a clean corpus: %v", h.Status, h.Diagnostics)
	}
	if h.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q, want complete", h.Coverage)
	}
	if h.IndexGeneration == "" || h.IndexGeneration != currentGeneration(t, workdir) {
		t.Errorf("health reports generation %q, published pointer says %q",
			h.IndexGeneration, currentGeneration(t, workdir))
	}
	if h.IndexWatermark != h.SourceWatermark {
		t.Errorf("index watermark %q != source watermark %q right after a build",
			h.IndexWatermark, h.SourceWatermark)
	}
	if h.RecordCount == 0 || h.IndexedCount != h.RecordCount {
		t.Errorf("record_count = %d, indexed_count = %d; a complete boundary indexed every record",
			h.RecordCount, h.IndexedCount)
	}
	if h.LastSuccess == nil {
		t.Error("last_success_at is unset after a successful publication")
	}
}

// TestUnreadableFileIsOneFailedRecord. One corrupt note must not cost a corpus
// its index, and a build that skipped it silently would report a complete
// boundary over an incomplete one — which is how a record that exists starts
// looking deleted.
func TestUnreadableFileIsOneFailedRecord(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t, corpus(t), nil) // the fixture keeps one non-UTF-8 file

	h := health(t, a)
	if h.FailedCount != 1 {
		t.Fatalf("failed_count = %d, want 1: %v", h.FailedCount, h.Diagnostics)
	}
	if h.Coverage != recall.IndexPartial {
		t.Errorf("coverage = %q, want partial while a record is missing", h.Coverage)
	}
	if h.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded", h.Status)
	}
	if h.IndexedCount+h.FailedCount != h.RecordCount {
		t.Errorf("indexed %d + failed %d != %d records", h.IndexedCount, h.FailedCount, h.RecordCount)
	}
	if _, ok := h.Diagnostics["failures"]; !ok {
		t.Error("diagnostics do not say which record failed")
	}

	// The rest of the corpus still answers, and says so honestly.
	resp := search(t, a, "corroboration counts units")
	if len(resp.Candidates) == 0 {
		t.Fatal("a partial index returned nothing")
	}
	if resp.Outcome != recall.SearchPartial {
		t.Errorf("outcome = %q, want partial from a generation built over a partial boundary", resp.Outcome)
	}
	if _, err := a.Expand(context.Background(), expandReq(resp.Candidates[0].Locator, recall.DetailExcerpt, 0)); err != nil {
		t.Errorf("expand from a partial generation: %v", err)
	}
}

// TestUnreadablePermissionsAreOneFailedRecord is the same rule for the other
// way a file refuses to be read. It is the case a document corpus actually
// meets: one note with wrong permissions in a directory of hundreds.
func TestUnreadablePermissionsAreOneFailedRecord(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions cannot make a file unreadable")
	}
	root := cleanCorpus(t)
	locked := filepath.Join(root, "projects", "recall", "status.md")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	a, workdir := newAdapter(t, root, nil)
	if currentGeneration(t, workdir) == "" {
		t.Fatal("an unreadable file stopped the build from publishing")
	}

	h := health(t, a)
	if h.FailedCount != 1 {
		t.Fatalf("failed_count = %d, want 1: %v", h.FailedCount, h.Diagnostics)
	}
	if h.Coverage != recall.IndexPartial {
		t.Errorf("coverage = %q, want partial", h.Coverage)
	}
	if resp := search(t, a, "corroboration counts units"); len(resp.Candidates) == 0 {
		t.Error("the readable part of the corpus answers nothing")
	}
}

// TestDeletedFileLeavesTheNextGeneration is the deletion obligation: a record
// proven gone by a complete boundary is excluded on publication, and its old
// locator expands to locator_expired rather than to a nearby record.
func TestDeletedFileLeavesTheNextGeneration(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, _ := newAdapter(t, root, nil)

	const doomed = "projects/clara/notes.md"
	before := search(t, a, "signals upstream projection memory")
	victim := firstFrom(t, before, doomed).Locator

	if _, err := a.Expand(context.Background(), expandReq(victim, recall.DetailFull, 0)); err != nil {
		t.Fatalf("expand before deletion: %v", err)
	}

	if err := os.Remove(filepath.Join(root, filepath.FromSlash(doomed))); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatalf("rebuild after deletion: %v", err)
	}

	after := search(t, a, "signals upstream projection memory")
	for _, c := range after.Candidates {
		if c.SourceRecordID == doomed {
			t.Errorf("%s survived deletion in the next generation", c.Locator.Local)
		}
	}

	_, err := a.Expand(context.Background(), expandReq(victim, recall.DetailFull, 0))
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("expand of a deleted record: %v, want locator_expired", err)
	}
}

// TestPublicationDropsSupersededGenerations. Superseded generations are not
// browsable history: keeping one would be a second answer to the same question,
// and anything still reading it would resurface deleted records.
func TestPublicationDropsSupersededGenerations(t *testing.T) {
	t.Parallel()
	a, workdir := newAdapter(t, cleanCorpus(t), nil)
	first := currentGeneration(t, workdir)

	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	second := currentGeneration(t, workdir)

	if first == second {
		t.Fatalf("pointer still names %q after a rebuild", first)
	}
	if got := indexEntries(t, workdir, genPrefix); len(got) != 1 || got[0] != second {
		t.Errorf("generations on disk = %v, want only the published %q", got, second)
	}
	if got := indexEntries(t, workdir, buildPrefix); len(got) != 0 {
		t.Errorf("staging directories left behind: %v", got)
	}
}

func TestCancelledRefreshStopsAndKeepsPublishedGeneration(t *testing.T) {
	t.Parallel()
	a, workdir := newAdapter(t, cleanCorpus(t), nil)
	published := currentGeneration(t, workdir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Refresh(ctx, protocol.RefreshParams{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled refresh error = %v, want context.Canceled", err)
	}
	if got := currentGeneration(t, workdir); got != published {
		t.Fatalf("cancelled refresh published %q over %q", got, published)
	}
	if got := indexEntries(t, workdir, buildPrefix); len(got) != 0 {
		t.Fatalf("cancelled refresh left staging directories: %v", got)
	}
	_, err = a.Refresh(context.Background(), protocol.RefreshParams{
		Full: true, Deadline: time.Now().Add(-time.Second),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired refresh error = %v, want context.DeadlineExceeded", err)
	}
	if got := currentGeneration(t, workdir); got != published {
		t.Fatalf("expired refresh published %q over %q", got, published)
	}
	h := health(t, a)
	if h.IndexGeneration != published || h.Status != recall.HealthHealthy {
		t.Fatalf("health after cancellation = %+v, want unchanged healthy generation %s", h, published)
	}
}

// TestFailedBuildKeepsThePreviousGenerationReadable fails the builder for real:
// a directory it cannot list. That is not a record-level failure — a directory
// nobody can enumerate makes the boundary itself unknown, and publishing it
// would make every file inside look deleted.
//
// What must survive: the previous generation stays published, keeps answering,
// and health says why it stopped moving forward.
func TestFailedBuildKeepsThePreviousGenerationReadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions cannot make a directory unreadable")
	}
	root := cleanCorpus(t)
	a, workdir := newAdapter(t, root, nil)
	published := currentGeneration(t, workdir)

	locked := filepath.Join(root, "projects", "recall")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// A failed build is reported through health, not as an error: a JSON-RPC
	// frame carries a result or an error and never both, so erroring here would
	// discard the health of the generation that is still answering.
	failed, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil {
		t.Fatalf("a failed build must not fail the refresh call: %v", err)
	}
	if failed.Status == recall.HealthHealthy {
		t.Errorf("refresh reports healthy after a build it could not complete: %v", failed.Diagnostics)
	}
	if got, _ := failed.Diagnostics["last_build_error"].(string); !strings.Contains(got, "projects/recall") {
		t.Errorf("diagnostics %q do not name what could not be read", got)
	}

	if got := currentGeneration(t, workdir); got != published {
		t.Fatalf("a failed build published %q over %q", got, published)
	}
	if got := indexEntries(t, workdir, genPrefix); len(got) != 1 {
		t.Errorf("generations on disk = %v, want only the previous one", got)
	}

	// The old generation still answers, from the index it already holds.
	resp := search(t, a, "corroboration counts units")
	if len(resp.Candidates) == 0 {
		t.Error("the previous generation stopped answering after a failed build")
	}

	h := health(t, a)
	if h.Status == recall.HealthHealthy {
		t.Errorf("status = healthy after a failed build: %v", h.Diagnostics)
	}
	if h.IndexGeneration != published {
		t.Errorf("health reports generation %q, want the still-published %q", h.IndexGeneration, published)
	}
	if _, ok := h.Diagnostics["last_build_error"]; !ok {
		t.Errorf("diagnostics do not report the failed build: %v", h.Diagnostics)
	}

	// And a build that can complete recovers without manual cleanup.
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatalf("recovery build: %v", err)
	}
	if got := currentGeneration(t, workdir); got == published {
		t.Error("the recovery build published nothing")
	}
	if got := indexEntries(t, workdir, buildPrefix); len(got) != 0 {
		t.Errorf("abandoned staging directories were not collected: %v", got)
	}
}

// TestKilledBuildKeepsThePreviousGenerationReadable kills a real builder
// process mid-build with SIGKILL.
//
// This is the case a failed build cannot prove: a killed process runs no
// deferred cleanup, no error path, and no rollback. Whatever survives is what
// the filesystem ordering guaranteed on its own, which is the entire claim
// behind atomic publication.
func TestKilledBuildKeepsThePreviousGenerationReadable(t *testing.T) {
	t.Parallel()
	root := cleanCorpus(t)
	a, workdir := newAdapter(t, root, nil)
	published := currentGeneration(t, workdir)
	if _, err := a.Search(context.Background(), recall.SearchRequest{Query: "corroboration", Limit: 5}); err != nil {
		t.Fatalf("search before the kill: %v", err)
	}
	_ = a.Close() // the child owns the workdir while it runs

	// A corpus large enough that a build is still running microseconds after it
	// starts. The first generation above was built over the small fixture, so
	// only the child pays for this.
	writeBulkCorpus(t, root, 4000)

	child := exec.Command(os.Args[0], "-test.run=TestKilledBuildKeepsThePreviousGenerationReadable")
	child.Env = append(os.Environ(), builderEnv+"="+root+string(os.PathListSeparator)+workdir)
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := child.Start(); err != nil {
		t.Fatalf("start builder: %v", err)
	}
	defer func() { _ = child.Wait() }()

	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "building" {
		t.Fatalf("builder never started: %q", lines.Text())
	}

	// Kill as soon as a staging directory exists: the build is provably past
	// its first write and provably before its publication.
	deadline := time.Now().Add(30 * time.Second)
	for len(indexEntries(t, workdir, buildPrefix)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no staging directory appeared; the builder never wrote anything")
		}
		time.Sleep(200 * time.Microsecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill builder: %v", err)
	}
	_ = child.Wait()

	if got := currentGeneration(t, workdir); got != published {
		// The kill lost the race against a build that finished anyway. Nothing
		// is wrong, but nothing was interrupted either, so there is no
		// interruption to assert on.
		t.Skipf("the builder published %q before the kill landed; nothing was interrupted", got)
	}
	if got := indexEntries(t, workdir, buildPrefix); len(got) == 0 {
		t.Fatal("no staging directory survived the kill; the build was not actually interrupted")
	}

	// A fresh adapter over the same workdir — the restart a supervisor would
	// perform — opens the previous generation and answers from it.
	revived := docs.New()
	t.Cleanup(func() { _ = revived.Close() })
	if _, err := revived.Initialize(context.Background(), config(root, workdir, nil)); err != nil {
		t.Fatalf("handshake after the kill: %v", err)
	}
	if got := currentGeneration(t, workdir); got != published {
		t.Fatalf("the handshake replaced the generation %q with %q", published, got)
	}

	resp := search(t, revived, "corroboration counts units")
	if len(resp.Candidates) == 0 {
		t.Error("the surviving generation answers nothing")
	}

	h := health(t, revived)
	if h.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded: the corpus has 4000 records this generation never saw", h.Status)
	}
	if stale, _ := h.Diagnostics["stale"].(bool); !stale {
		t.Errorf("health does not mark the generation stale: %v", h.Diagnostics)
	}
	if h.IndexWatermark == h.SourceWatermark {
		t.Errorf("watermarks agree (%q) after the corpus grew by 4000 files", h.IndexWatermark)
	}
	if h.IndexGeneration != published {
		t.Errorf("health reports generation %q, want the still-published %q", h.IndexGeneration, published)
	}
	if h.RecordCount <= h.IndexedCount {
		t.Errorf("record_count %d does not exceed indexed_count %d after the corpus grew",
			h.RecordCount, h.IndexedCount)
	}
}

// writeBulkCorpus adds enough documents that a build takes long enough to be
// interrupted. The content varies per file so no two chunks are identical.
func writeBulkCorpus(t *testing.T, root string, files int) {
	t.Helper()
	dir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("bulk dir: %v", err)
	}
	var body strings.Builder
	for i := range files {
		body.Reset()
		fmt.Fprintf(&body, "# Bulk Note %d\n\nA generated note about topic %d.\n", i, i%97)
		for section := range 12 {
			fmt.Fprintf(&body, "\n## Section %d\n\nnote%d section%d filler text about indexing and ranking.\n",
				section, i, section)
		}
		name := filepath.Join(dir, fmt.Sprintf("note-%05d.md", i))
		if err := os.WriteFile(name, []byte(body.String()), 0o644); err != nil {
			t.Fatalf("bulk file: %v", err)
		}
	}
}
