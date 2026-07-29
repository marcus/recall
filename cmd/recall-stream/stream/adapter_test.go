package stream_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-stream/stream"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Two schema versions, two upstream systems, one of them unmapped. Every test
// below reads from this unless it needs to break something specific.
const corpus = `{"schema":1,"id":"sig-0001","kind":"event","event_time":"2026-05-02T09:12:00Z","observed_at":"2026-05-02T09:12:04Z","system":"tasks","ref":"td-f62256","correlation":"td-f62256","title":"Task td-f62256 moved to NEXT","body":"Ship the reference stream adapter."}
{"schema":1,"id":"sig-0002","kind":"message","event_time":"2026-05-02T09:20:00Z","observed_at":"2026-05-02T09:20:03Z","system":"slack","ref":"C0/1746177600.0002","title":"Marcus posted in #recall","body":"Lineage edges are the interesting half."}
{"schema":2,"id":"sig-0003","kind":"event","event_time":"2026-05-02T09:25:00Z","observed_at":"2026-05-02T09:25:02Z","system":"jira","ref":"REC-118","title":"REC-118 in review","text":"Reviewer asked for transcripts.","actor":"marcus"}
`

// fixture writes a stream file and returns the directory holding it.
func fixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "signals.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func settings(extra map[string]any) map[string]any {
	set := map[string]any{
		"files":    []any{"signals.jsonl"},
		"upstream": map[string]any{"tasks": "tasks", "jira": "jira-work"},
	}
	for k, v := range extra {
		set[k] = v
	}
	return set
}

// start returns an initialized adapter and its workdir.
func start(t *testing.T, location string, set map[string]any) (*stream.Adapter, string) {
	t.Helper()
	workdir := t.TempDir()
	a := stream.New(stream.Options{})
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: workdir, SourceID: "signals", Location: location, Settings: set,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return a, workdir
}

func TestInitializeNegotiatesOrRefuses(t *testing.T) {
	// The handshake is the only place a version is agreed. A range with no
	// overlap must fail here; degrading to a version neither end implements is
	// what the handshake exists to prevent.
	tests := []struct {
		name     string
		min, max int
		wantErr  bool
	}{
		{name: "exact match", min: 1, max: 1},
		{name: "range covering the only version", min: 1, max: 4},
		{name: "range entirely above this build", min: 2, max: 3, wantErr: true},
	}
	dir := fixture(t, corpus)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := stream.New(stream.Options{})
			defer a.Close() //nolint:errcheck // test cleanup
			manifest, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: tc.min, ProtocolVersionMax: tc.max,
				Workdir: t.TempDir(), SourceID: "signals", Location: dir, Settings: settings(nil),
			})
			if tc.wantErr {
				var verr *protocol.VersionError
				if !errors.As(err, &verr) {
					t.Fatalf("want a version error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if manifest.ProtocolVersion != 1 {
				t.Errorf("negotiated version %d, want 1", manifest.ProtocolVersion)
			}
		})
	}
}

func TestManifestDeclaresOnlyWhatItCanDo(t *testing.T) {
	a, _ := start(t, fixture(t, corpus), settings(nil))
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "signals", Location: fixture(t, corpus), Settings: settings(nil),
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// as_of is the field the Tasks adapter had to refuse. Here event_time is
	// real record history, so filter is honest — but snapshot would not be:
	// a record about an early event can be appended at any later time.
	if manifest.AsOfSupport != recall.AsOfFilter {
		t.Errorf("as_of_support = %q, want %q", manifest.AsOfSupport, recall.AsOfFilter)
	}
	// checkpoint means recall/refresh is served. Declaring it without serving
	// it would be a capability nothing can invoke.
	if !manifest.Can(recall.CapCheckpoint) {
		t.Error("manifest does not declare the checkpoint capability")
	}
	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{Deadline: soon()}); err != nil {
		t.Errorf("refresh: %v", err)
	}
	if manifest.SettingsSchema == nil {
		t.Error("manifest declares no settings_schema; recall doctor could not validate a configuration")
	}
}

func TestSettingsAreValidated(t *testing.T) {
	// A setting with no code path behind it is a defect, not a tolerance, so
	// an unknown key fails the handshake rather than being ignored.
	tests := []struct {
		name string
		set  map[string]any
		want string
	}{
		{
			name: "unknown key",
			set:  map[string]any{"files": []any{"signals.jsonl"}, "cursor_file": "elsewhere"},
			want: "unknown field",
		},
		{
			name: "upstream mapping to a name containing the locator separator",
			set:  map[string]any{"upstream": map[string]any{"tasks": "tasks:extra"}},
			want: "unusable source_id",
		},
		{
			name: "negative bound",
			set:  map[string]any{"max_candidates": -1},
			want: "cannot be negative",
		},
		{
			name: "file escaping the configured location",
			set:  map[string]any{"files": []any{"../../etc/passwd"}},
			want: "outside the configured location",
		},
	}
	dir := fixture(t, corpus)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := stream.New(stream.Options{})
			defer a.Close() //nolint:errcheck // test cleanup
			_, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: 1, ProtocolVersionMax: 1,
				Workdir: t.TempDir(), SourceID: "signals", Location: dir, Settings: tc.set,
			})
			if err == nil {
				t.Fatal("want a handshake failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if !errors.Is(err, protocol.ErrInvalidParams) {
				t.Errorf("error %v is not invalid_params", err)
			}
		})
	}
}

func TestHandshakeRefusesToRunWithoutAWorkdir(t *testing.T) {
	// The workdir is Recall's to provide. Without one there is nowhere this
	// adapter may legitimately write, and inventing a location is the thing
	// the field exists to prevent.
	a := stream.New(stream.Options{})
	defer a.Close() //nolint:errcheck // test cleanup
	_, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		SourceID: "signals", Location: fixture(t, corpus), Settings: settings(nil),
	})
	if err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Fatalf("want a workdir failure, got %v", err)
	}
}

func TestWritesNothingOutsideTheWorkdir(t *testing.T) {
	dir := fixture(t, corpus)
	a, workdir := start(t, dir, settings(nil))
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}

	written := names(t, workdir)
	if len(written) != 1 || written[0] != "cursor.json" {
		t.Errorf("workdir holds %v, want only cursor.json", written)
	}
	if source := names(t, dir); len(source) != 1 || source[0] != "signals.jsonl" {
		t.Errorf("the source directory gained files: %v", source)
	}
}

func TestHealthReportsPartialCoverageHonestly(t *testing.T) {
	// A record that failed to parse is unknown, not absent. Reporting healthy
	// with a complete coverage would make the gap invisible, and a recent
	// index timestamp alone is not health.
	broken := corpus + "{\"schema\":9,\"id\":\"sig-0009\",\"event_time\":\"2026-05-02T10:00:00Z\"}\nnot json at all\n"
	a, _ := start(t, fixture(t, broken), settings(nil))

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	switch {
	case health.Status != recall.HealthDegraded:
		t.Errorf("status = %q, want degraded", health.Status)
	case health.Coverage != recall.IndexPartial:
		t.Errorf("coverage = %q, want partial", health.Coverage)
	case health.FailedCount != 2:
		t.Errorf("failed_count = %d, want 2", health.FailedCount)
	case health.RecordCount != health.IndexedCount+health.FailedCount:
		t.Errorf("record_count %d does not account for %d indexed and %d failed",
			health.RecordCount, health.IndexedCount, health.FailedCount)
	}
}

func TestUnreadableSourceIsNeverAnEmptySuccess(t *testing.T) {
	// Invariant 2. An unreachable source and a source with no matches must
	// never be the same answer.
	dir := t.TempDir()
	a, _ := start(t, dir, settings(nil))

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "anything", Limit: 10, Deadline: soon(),
	})
	if err == nil {
		t.Fatal("want an error for a source that cannot be read")
	}
	if !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Errorf("error %v is not source_unavailable", err)
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %q, want anything but success", resp.Outcome)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("diagnostic %q leaks a local path", err)
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable || health.Coverage != recall.IndexUnknown {
		t.Errorf("health = %q/%q, want unavailable/unknown", health.Status, health.Coverage)
	}
}

func TestGenerationsSurviveARestart(t *testing.T) {
	// The checkpoint's whole job. A second process over the same workdir must
	// not reuse a generation id the first one already published, or two
	// different builds would answer under one name in saved evidence.
	dir := fixture(t, corpus)
	workdir := t.TempDir()

	first := stream.New(stream.Options{})
	cfg := adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: workdir, SourceID: "signals", Location: dir, Settings: settings(nil),
	}
	if _, err := first.Initialize(context.Background(), cfg); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	before, err := first.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := stream.New(stream.Options{})
	defer second.Close() //nolint:errcheck // test cleanup
	if _, err := second.Initialize(context.Background(), cfg); err != nil {
		t.Fatalf("re-initialize: %v", err)
	}
	after, err := second.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if before.IndexGeneration != "gen-1" || after.IndexGeneration != "gen-2" {
		t.Fatalf("generations %q then %q, want gen-1 then gen-2",
			before.IndexGeneration, after.IndexGeneration)
	}
	// The records themselves are memory-resident, so the second process must
	// have read the file whole rather than resuming from the stored offset.
	if before.IndexedCount != after.IndexedCount {
		t.Errorf("indexed %d records, then %d after a restart",
			before.IndexedCount, after.IndexedCount)
	}
}

func TestRewrittenStreamIsReportedAndRebuilt(t *testing.T) {
	// An append-only stream that shrank broke its own contract. Every offset
	// behind the new end is meaningless, so the scan starts over — and says so,
	// because a caller comparing watermarks across that boundary deserves to
	// know why they do not line up.
	dir := fixture(t, corpus)
	a, _ := start(t, dir, settings(nil))
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}

	path := filepath.Join(dir, "signals.jsonl")
	rewritten := strings.SplitAfter(corpus, "\n")[0]
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Diagnostics["stream_rewritten"] != true {
		t.Errorf("diagnostics %v do not report the rewrite", health.Diagnostics)
	}
	if health.IndexedCount != 1 {
		t.Errorf("indexed %d records after the rewrite, want 1", health.IndexedCount)
	}
	if health.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded", health.Status)
	}
}

func TestUnwritableWorkdirIsReportedNotFatal(t *testing.T) {
	// The generation is published before the checkpoint is written, so a
	// workdir that cannot be written still answers correctly — it only loses
	// the ability to tell the next process what this one consumed. That is a
	// degradation, and saying so is the difference between a known gap and a
	// silent one.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, workdir := start(t, fixture(t, corpus), settings(nil))
	if err := os.Chmod(workdir, 0o500); err != nil {
		t.Fatalf("chmod workdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workdir, 0o700) })

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.IndexedCount != 3 {
		t.Errorf("indexed %d records, want 3: the answer must not depend on the checkpoint",
			health.IndexedCount)
	}
	if health.Diagnostics["checkpoint_unwritable"] != true {
		t.Errorf("diagnostics %v do not report the unwritable workdir", health.Diagnostics)
	}
	if health.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded", health.Status)
	}
}

func TestUseAfterCloseFails(t *testing.T) {
	a, _ := start(t, fixture(t, corpus), settings(nil))
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := a.Search(context.Background(), recall.SearchRequest{Query: "x", Limit: 1, Deadline: soon()}); !errors.Is(err, adapter.ErrClosed) {
		t.Errorf("search after close: %v, want ErrClosed", err)
	}
	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "signals", Local: "v1/sig-0001"}, Detail: recall.DetailFull, Deadline: soon(),
	}); !errors.Is(err, adapter.ErrClosed) {
		t.Errorf("expand after close: %v, want ErrClosed", err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func soon() time.Time { return time.Now().Add(time.Minute) }
