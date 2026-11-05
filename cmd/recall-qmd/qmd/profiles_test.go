package qmd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// A real protocol process, using a disk-backed engine seam instead of models.
// Separate profiles cannot accidentally share an in-memory engine or mutex.
func TestSharedProfileAdapterProcess(t *testing.T) {
	if os.Getenv("RECALL_PROFILE_HELPER") != "1" {
		return
	}
	base := os.Getenv("RECALL_PROFILE_FIXTURE")
	a := &captureSharedConfig{Adapter: qmd.New(qmd.Options{Runner: profileEngine{base}})}
	if err := adapter.Serve(context.Background(), os.Stdin, os.Stdout, a); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type captureSharedConfig struct{ adapter.Adapter }

func (a *captureSharedConfig) Initialize(ctx context.Context, cfg adapter.Config) (recall.Manifest, error) {
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(cfg.Workdir, "handshake.json"), raw, 0600); err != nil {
		return recall.Manifest{}, err
	}
	return a.Adapter.Initialize(ctx, cfg)
}

type profileEngine struct{ base string }

func (e profileEngine) Kind() string           { return "live" }
func (e profileEngine) Now() (time.Time, bool) { return time.Time{}, false }
func (e profileEngine) Root() (string, bool)   { return "", false }
func (e profileEngine) Run(ctx context.Context, args ...string) (qmd.Result, error) {
	result := func(s string) (qmd.Result, error) { return qmd.Result{Stdout: []byte(s)}, nil }
	root := filepath.Join(e.base, "corpus")
	collection := ""
	for i, arg := range args {
		if arg == "-c" {
			collection = args[i+1]
		}
	}
	pathFile := func(name string) string { return filepath.Join(e.base, name+".path") }
	switch args[0] {
	case "--version":
		return result("qmd 2.5.3")
	case "collection":
		if args[1] == "add" {
			// Creation must happen only once even under competing refreshes.
			f, err := os.OpenFile(pathFile(args[4]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return qmd.Result{}, err
			}
			_, err = f.WriteString(args[2])
			f.Close()
			return resultErr("Created", err)
		}
		if args[2] == "fixture" {
			return result(collectionText(root, "fixture"))
		}
		data, err := os.ReadFile(pathFile(args[2]))
		if os.IsNotExist(err) {
			return result("Collection not found: " + args[2])
		}
		if err != nil {
			return qmd.Result{}, err
		}
		return result(collectionText(string(data), args[2]))
	case "status":
		text := statusText(root, "fixture", 2, 2, 1)
		paths, _ := filepath.Glob(filepath.Join(e.base, "*.path"))
		for _, p := range paths {
			name := strings.TrimSuffix(filepath.Base(p), ".path")
			data, _ := os.ReadFile(filepath.Join(e.base, name+".index"))
			var entries map[string]string
			json.Unmarshal(data, &entries)
			text += statusText(root, name, 2, 2, len(entries))
		}
		return result(text)
	case "update", "embed":
		transaction := filepath.Join(e.base, "transaction")
		if args[0] == "update" && collection == "fixture" {
			f, err := os.OpenFile(transaction, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				os.WriteFile(filepath.Join(e.base, "overlap"), []byte("overlapping refresh"), 0600)
				return qmd.Result{}, err
			}
			f.Close()
		}
		if args[0] == "embed" && collection != "fixture" {
			if _, err := os.Stat(filepath.Join(e.base, "gate")); err == nil {
				os.WriteFile(filepath.Join(e.base, "blocked"), nil, 0600)
				for {
					if _, err := os.Stat(filepath.Join(e.base, "gate")); os.IsNotExist(err) {
						break
					}
					select {
					case <-ctx.Done():
						os.Remove(transaction)
						return qmd.Result{}, ctx.Err()
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			os.Remove(transaction)
			if _, err := os.Stat(filepath.Join(e.base, "fail")); err == nil {
				return qmd.Result{ExitCode: 1, Stderr: []byte("failed embed")}, nil
			}
		}
		if args[0] == "update" && collection != "fixture" {
			mirror, err := os.ReadFile(pathFile(collection))
			if err != nil {
				return qmd.Result{}, err
			}
			entries := map[string]string{}
			err = filepath.WalkDir(string(mirror), func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(string(mirror), p)
				data, err := os.ReadFile(p)
				entries[filepath.ToSlash(rel)] = string(data)
				return err
			})
			if err != nil {
				return qmd.Result{}, err
			}
			raw, _ := json.Marshal(entries)
			if err = os.WriteFile(filepath.Join(e.base, collection+".index"), raw, 0600); err != nil {
				return qmd.Result{}, err
			}
		}
		return result("Done")
	case "vsearch", "search", "query":
		if collection == "fixture" {
			return result("[]")
		}
		data, err := os.ReadFile(filepath.Join(e.base, collection+".index"))
		if err != nil {
			return qmd.Result{}, err
		}
		var entries map[string]string
		json.Unmarshal(data, &entries)
		var hits []map[string]any
		for rel, text := range entries {
			hits = append(hits, map[string]any{"docid": "#semantic", "score": 0.48, "file": "qmd://" + collection + "/" + strings.ReplaceAll(rel, ".html.md", "-html.md"), "line": 3, "title": "Rainwater reservoir", "snippet": fmt.Sprintf("@@ -3,%d @@\n%s", max(1, strings.Count(text, "\n")-2), strings.TrimPrefix(text, "# Rainwater reservoir\n\n"))})
		}
		if len(hits) == 0 {
			return result("[]")
		}
		raw, _ := json.Marshal(hits)
		return result(string(raw))
	}
	return qmd.Result{}, fmt.Errorf("unexpected invocation %v", args)
}
func resultErr(text string, err error) (qmd.Result, error) {
	return qmd.Result{Stdout: []byte(text)}, err
}

func profileFixture(t *testing.T) (string, *config.Config) {
	t.Helper()
	base := t.TempDir()
	writeDerived(t, filepath.Join(base, "corpus"), "a.md", "# Bread\nMix flour with water.")
	writeDerived(t, filepath.Join(base, "corpus"), "b.html", reservoirHTML)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`
[defaults]
profile = "work"
timeout_ms = 10000
[adapters.qmd]
command = %q
args = ["-test.run=^TestSharedProfileAdapterProcess$"]
freshness_modes = ["indexed"]
[adapters.qmd.env]
RECALL_PROFILE_HELPER = "1"
RECALL_PROFILE_FIXTURE = %q
[[sources]]
source_uid = "01SHAREDQMD"
source_id = "semantic"
adapter = "qmd"
location = %q
freshness_mode = "indexed"
sensitivity = "internal"
[sources.settings]
collection = "fixture"
mode = "vector"
derive_extensions = [".html"]
derived_collection = "html-view"
[profiles.work]
sources = ["semantic"]
max_sensitivity = "internal"
[profiles.docs]
sources = ["semantic"]
max_sensitivity = "internal"
`, exe, base, filepath.Join(base, "corpus"))
	writeDerived(t, filepath.Join(base, "config", "recall"), "config.toml", raw)
	cfg, err := config.Load(config.Options{Paths: config.Paths{ConfigHome: filepath.Join(base, "config"), StateHome: filepath.Join(base, "state"), CacheHome: filepath.Join(base, "cache")}})
	if err != nil {
		t.Fatal(err)
	}
	return base, cfg
}
func profileApp(t *testing.T, cfg *config.Config, profile string) *app.App {
	t.Helper()
	core, reg, err := app.Build(app.BuildOptions{Config: cfg, StateDir: cfg.Paths.StateDir(profile)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return core
}
func profileRefresh(t *testing.T, core *app.App, profile string) {
	t.Helper()
	r, err := core.Refresh(t.Context(), recall.RefreshRequest{Profile: profile})
	if err != nil || r.Outcome != recall.RefreshSucceeded {
		t.Fatalf("refresh: %+v %v", r, err)
	}
}
func profileQuery(t *testing.T, core *app.App, profile string) recall.QueryResponse {
	t.Helper()
	r, err := core.Query(t.Context(), recall.QueryRequest{Profile: profile, Query: "where is precipitation collected underground", Mode: recall.ModeExplicit, Budget: recall.Budget{LatencyMS: 2000}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func waitProfileFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("missing marker %s", path)
}

func TestConfiguredProfilesShareMirrorReceiptAndSerializeRefresh(t *testing.T) {
	base, cfg := profileFixture(t)
	work := profileApp(t, cfg, "work")
	docs := profileApp(t, cfg, "docs")
	profileRefresh(t, work, "work")
	if r := profileQuery(t, docs, "docs"); len(r.Results) != 1 {
		t.Fatalf("second profile cannot query first refresh: %+v", r)
	}
	// The actual config -> registry -> external protocol handshake carries one
	// explicit identity while the ordinary workdirs remain profile-owned.
	for _, profile := range []string{"work", "docs"} {
		raw, err := os.ReadFile(filepath.Join(cfg.Paths.StateDir(profile), "adapters", "01SHAREDQMD", "handshake.json"))
		if err != nil {
			t.Fatal(err)
		}
		var init protocol.InitializeParams
		json.Unmarshal(raw, &init)
		if init.SharedWorkdir != cfg.Paths.SharedSourceDir("01SHAREDQMD") {
			t.Fatalf("wrong shared workdir: %+v", init)
		}
	}
	shared := cfg.Paths.SharedSourceDir("01SHAREDQMD")
	receipt := filepath.Join(shared, "qmd-derived.json")
	writeDerived(t, base, "gate", "")
	done := make(chan recall.RefreshResponse, 2)
	go func() { r, _ := work.Refresh(t.Context(), recall.RefreshRequest{Profile: "work"}); done <- r }()
	waitProfileFile(t, filepath.Join(base, "blocked"))
	raw, _ := os.ReadFile(receipt)
	if strings.Contains(string(raw), `"ready": true`) {
		t.Fatal("receipt remains ready during maintenance")
	}
	// A cancelled waiter must not change the writer's invalid receipt.
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	r, err := docs.Refresh(ctx, recall.RefreshRequest{Profile: "docs"})
	cancel()
	if err != nil || r.Sources[0].Reason != recall.RefreshTimedOut {
		t.Fatalf("wait cancellation: %+v %v", r, err)
	}
	after, _ := os.ReadFile(receipt)
	if string(after) != string(raw) {
		t.Fatal("cancelled waiter changed receipt")
	}
	// Use a fresh process after the timed-out protocol session, then compete.
	docs2 := profileApp(t, cfg, "docs")
	go func() { r, _ := docs2.Refresh(t.Context(), recall.RefreshRequest{Profile: "docs"}); done <- r }()
	queryApp := profileApp(t, cfg, "docs")
	qctx, qcancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	qr, qerr := queryApp.Query(qctx, recall.QueryRequest{Profile: "docs", Query: "precipitation", Mode: recall.ModeExplicit, Budget: recall.Budget{LatencyMS: 100}, Limit: 10})
	qcancel()
	if len(qr.Results) > 0 {
		t.Fatalf("query during refresh published derived hits: %+v %v", qr, qerr)
	}
	os.Remove(filepath.Join(base, "gate"))
	for range 2 {
		select {
		case r := <-done:
			if r.Outcome != recall.RefreshSucceeded {
				t.Fatalf("competing refresh: %+v", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("refresh stuck")
		}
	}
	if _, err := os.Stat(filepath.Join(base, "overlap")); !os.IsNotExist(err) {
		t.Fatal("cross-process refresh overlapped")
	}
	if r := profileQuery(t, docs2, "docs"); len(r.Results) != 1 {
		t.Fatalf("post-refresh query: %+v", r)
	}
	paths, _ := filepath.Glob(filepath.Join(base, "*.path"))
	if len(paths) != 1 {
		t.Fatalf("derived collections: %v", paths)
	}
	path, _ := os.ReadFile(paths[0])
	if string(path) != filepath.Join(shared, "qmd-derived") {
		t.Fatalf("wrong collection mirror %s", path)
	}
	for _, profile := range []string{"work", "docs"} {
		if _, err := os.Stat(filepath.Join(cfg.Paths.StateDir(profile), "qmd-derived")); !os.IsNotExist(err) {
			t.Fatal("profile mirror created")
		}
		if _, err := os.Stat(filepath.Join(cfg.Paths.StateDir(profile), "adapters", "01SHAREDQMD", "qmd-derived.json")); !os.IsNotExist(err) {
			t.Fatal("profile receipt created")
		}
	}
}

func TestConfiguredProfileFailedAndCancelledRefreshRecovery(t *testing.T) {
	base, cfg := profileFixture(t)
	work := profileApp(t, cfg, "work")
	profileRefresh(t, work, "work")
	writeDerived(t, base, "fail", "")
	r, err := work.Refresh(t.Context(), recall.RefreshRequest{Profile: "work"})
	if err != nil || r.Outcome == recall.RefreshSucceeded {
		t.Fatalf("failed embed: %+v %v", r, err)
	}
	docs := profileApp(t, cfg, "docs")
	if r := profileQuery(t, docs, "docs"); len(r.Results) != 0 {
		t.Fatalf("failed shared receipt served: %+v", r)
	}
	os.Remove(filepath.Join(base, "fail"))
	profileRefresh(t, docs, "docs")
	writeDerived(t, base, "gate", "")
	done := make(chan recall.RefreshResponse, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { r, _ := work.Refresh(ctx, recall.RefreshRequest{Profile: "work"}); done <- r }()
	waitProfileFile(t, filepath.Join(base, "blocked"))
	cancel()
	<-done
	fresh := profileApp(t, cfg, "docs")
	if r := profileQuery(t, fresh, "docs"); len(r.Results) != 0 {
		t.Fatalf("cancelled shared receipt served: %+v", r)
	}
	os.Remove(filepath.Join(base, "gate"))
	profileRefresh(t, fresh, "docs")
	if r := profileQuery(t, work, "work"); len(r.Results) != 1 {
		t.Fatalf("cross-profile recovery: %+v", r)
	}
}

func TestConfiguredProfileLegacyMirrorSafeRecovery(t *testing.T) {
	base, cfg := profileFixture(t)
	oldMirror := filepath.Join(cfg.Paths.StateDir("work"), "qmd-derived", "01SHAREDQMD")
	inst, _ := cfg.Source("semantic")
	legacy := qmd.New(qmd.Options{Runner: profileEngine{base}})
	_, err := legacy.Initialize(t.Context(), adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, SourceID: inst.ID, Location: inst.Location, Workdir: filepath.Join(cfg.Paths.StateDir("work"), "adapters", string(inst.UID)), Settings: inst.Settings})
	if err != nil {
		t.Fatal(err)
	}
	// Omitting shared_workdir exercises exactly the v0.7.0 artifact layout,
	// including a successful receipt whose corpus/settings really do agree.
	health, err := legacy.Refresh(t.Context(), protocol.RefreshParams{})
	legacy.Close()
	if err != nil || health.Status != recall.HealthHealthy {
		t.Fatalf("legacy refresh: %+v %v", health, err)
	}
	work := profileApp(t, cfg, "work")
	r, err := work.Refresh(t.Context(), recall.RefreshRequest{Profile: "work"})
	if err != nil || r.Outcome != recall.RefreshFailed {
		t.Fatalf("old collection repointed: %+v %v", r, err)
	}
	detail := r.Sources[0].DiagnosticDetail
	if !strings.Contains(detail, oldMirror) || !strings.Contains(detail, filepath.Join(cfg.Paths.SharedSourceDir("01SHAREDQMD"), "qmd-derived")) || !strings.Contains(detail, "unused derived_collection") {
		t.Fatalf("missing full paths/recovery: %s", detail)
	}
	data, _ := os.ReadFile(filepath.Join(base, "html-view.path"))
	if string(data) != oldMirror {
		t.Fatal("legacy collection changed")
	}
	// Follow the documented safe recovery; preserve the old collection and use
	// an unused name, never infer ownership from similar-looking paths.
	inst.Settings["derived_collection"] = "html-shared"
	recovered := profileApp(t, cfg, "docs")
	profileRefresh(t, recovered, "docs")
	if r := profileQuery(t, recovered, "docs"); len(r.Results) != 1 {
		t.Fatalf("migration recovery query: %+v", r)
	}
}

func TestConfiguredProfilesConcurrentColdRefresh(t *testing.T) {
	base, cfg := profileFixture(t)
	work, docs := profileApp(t, cfg, "work"), profileApp(t, cfg, "docs")
	ready := make(chan struct{})
	done := make(chan recall.RefreshResponse, 2)
	for _, item := range []struct {
		core    *app.App
		profile string
	}{{work, "work"}, {docs, "docs"}} {
		go func() {
			<-ready
			r, _ := item.core.Refresh(t.Context(), recall.RefreshRequest{Profile: item.profile})
			done <- r
		}()
	}
	close(ready)
	for range 2 {
		select {
		case r := <-done:
			if r.Outcome != recall.RefreshSucceeded {
				t.Fatalf("cold concurrent refresh: %+v", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cold refresh stuck")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(base, "*.path"))
	if len(paths) != 1 {
		t.Fatalf("derived collections: %v", paths)
	}
	if _, err := os.Stat(filepath.Join(base, "overlap")); !os.IsNotExist(err) {
		t.Fatal("cold refreshes overlapped")
	}
	for _, item := range []struct {
		core    *app.App
		profile string
	}{{work, "work"}, {docs, "docs"}} {
		if r := profileQuery(t, item.core, item.profile); len(r.Results) != 1 {
			t.Fatalf("cold profile query: %+v", r)
		}
	}
}
