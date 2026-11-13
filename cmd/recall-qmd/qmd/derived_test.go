package qmd_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// The fake external engine indexes the actual mirror bytes on update. Searches
// replay a semantic score over that snapshot, so edits/deletions cannot pass
// merely by returning today's file without exercising refresh.
type mirrorEngine struct {
	root, mirror  string
	runner        *fakeRunner
	exists        bool
	wrong         string
	fail          string
	indexed       map[string]string
	onSearch      func()
	primaryHit    bool
	preservePaths bool
}

func newMirrorEngine(root, mirror string) *mirrorEngine {
	e := &mirrorEngine{root: root, mirror: mirror, indexed: map[string]string{}}
	e.runner = &fakeRunner{fallback: func(args []string) (qmd.Result, error) {
		result := func(s string) (qmd.Result, error) { return qmd.Result{Stdout: []byte(s)}, nil }
		collection := ""
		for i, a := range args {
			if a == "-c" {
				collection = args[i+1]
			}
		}
		switch args[0] {
		case "--version":
			return result("qmd 2.5.3")
		case "collection":
			if args[1] == "add" {
				e.exists = true
				return result("Created")
			}
			if args[2] == "fixture" {
				return result(collectionText(root, "fixture"))
			}
			if !e.exists {
				return result("Collection not found: html-view")
			}
			p := mirror
			if e.wrong != "" {
				p = e.wrong
			}
			return result(collectionText(p, "html-view"))
		case "status":
			return result(statusText(root, "fixture", 1+len(e.indexed), 1+len(e.indexed), 1) + statusText(root, "html-view", 1+len(e.indexed), 1+len(e.indexed), len(e.indexed)))
		case "update", "embed":
			if args[0]+":"+collection == e.fail {
				return qmd.Result{ExitCode: 1, Stderr: []byte("index unavailable")}, nil
			}
			if args[0] == "update" && collection == "html-view" {
				e.indexed = map[string]string{}
				err := filepath.WalkDir(mirror, func(p string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() {
						return nil
					}
					rel, err := filepath.Rel(mirror, p)
					if err != nil {
						return err
					}
					data, err := os.ReadFile(p)
					e.indexed[filepath.ToSlash(rel)] = string(data)
					return err
				})
				if err != nil {
					return qmd.Result{}, err
				}
			}
			return result("Done")
		case "search", "vsearch", "query":
			if e.onSearch != nil {
				f := e.onSearch
				e.onSearch = nil
				f()
			}
			if collection == "fixture" {
				if e.primaryHit {
					return result(`[{"docid":"#primary","score":0.45,"file":"qmd://fixture/a.md","line":1,"title":"Bread","snippet":"@@ -1,2 @@\n# Bread\nMix flour with water."}]`)
				}
				return result("[]")
			}
			var hits []map[string]any
			for rel, text := range e.indexed {
				if strings.HasPrefix(rel, "b.html") {
					virtual := strings.ReplaceAll(rel, ".html.md", "-html.md")
					if e.preservePaths {
						virtual = rel
					}
					hits = append(hits, map[string]any{"docid": "#semantic", "score": 0.48, "file": "qmd://html-view/" + virtual, "line": 3, "title": "Rainwater reservoir", "snippet": fmt.Sprintf("@@ -3,%d @@\n%s", max(1, strings.Count(text, "\n")-2), strings.TrimPrefix(text, "# Rainwater reservoir\n\n"))})
				}
			}
			if hits == nil {
				return result("[]")
			}
			raw, _ := json.Marshal(hits)
			return result(string(raw))
		}
		return qmd.Result{}, fmt.Errorf("unexpected invocation %v", args)
	}}
	return e
}

const reservoirHTML = "<!doctype html>\n<html><head><title>Rainwater reservoir</title>\n<style>csspoison</style></head>\n<body>\n<script>scriptpoison</script>\n\n\n<h1 id=\"storage\">Rainwater reservoir</h1>\n<p>Rainwater accumulates in an underground cistern.</p>\n<ul><li>Keep the lid closed.</li></ul>\n</body></html>\n"

func writeDerived(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func derivedFixture(t *testing.T, mode string) (*qmd.Adapter, *mirrorEngine, adapter.Config) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "corpus")
	writeDerived(t, root, "a.md", "# Bread\nMix flour with water.")
	writeDerived(t, root, "b.html", reservoirHTML)
	cfg := adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, SourceID: "semantic", Location: root, Workdir: filepath.Join(base, "state", "adapters", "source-uid"), Settings: map[string]any{"collection": "fixture", "mode": mode, "derive_extensions": []string{".html"}, "derived_collection": "html-view", "sibling_preference": []string{".md", ".html"}, "exclude_globs": []string{"**/mockups/**", "*.template.html"}}}
	mirror := filepath.Join(base, "state", "qmd-derived", "source-uid")
	engine := newMirrorEngine(root, mirror)
	a := qmd.New(qmd.Options{Runner: engine.runner})
	if _, err := a.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a, engine, cfg
}
func refreshMirror(t *testing.T, a *qmd.Adapter) recall.Health {
	t.Helper()
	h, err := a.Refresh(t.Context(), protocol.RefreshParams{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func requireMirrorHit(t *testing.T, a *qmd.Adapter) recall.Candidate {
	t.Helper()
	r, err := searchOnce(t, a, "where is precipitation collected below ground")
	if err != nil || r.Outcome != recall.SearchSuccess || len(r.Candidates) != 1 {
		t.Fatalf("search: %+v %v", r, err)
	}
	return r.Candidates[0]
}

func TestDerivedRefreshSearchExpandAndFusion(t *testing.T) {
	a, e, cfg := derivedFixture(t, "vector")
	before := len(e.runner.invocations())
	r, err := searchOnce(t, a, "below ground precipitation")
	if err != nil || r.Outcome != recall.SearchPartial {
		t.Fatalf("unbuilt query: %+v %v", r, err)
	}
	for _, args := range e.runner.invocations()[before:] {
		if args[0] == "update" || args[0] == "embed" || (len(args) > 1 && args[1] == "add") {
			t.Fatalf("query mutated index: %v", args)
		}
	}
	if _, err := os.Stat(e.mirror); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("query created mirror: %v", err)
	}
	if h := refreshMirror(t, a); h.Status != recall.HealthHealthy || h.Diagnostics["mirrored_files"] != 1 {
		t.Fatalf("refresh: %+v", h)
	}
	raw, err := os.ReadFile(filepath.Join(e.mirror, "b.html.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# Rainwater reservoir\n") || strings.Contains(string(raw), "poison") {
		t.Fatalf("mirror = %s", raw)
	}
	c := requireMirrorHit(t, a)
	if c.SourceRecordID != "b.html" || c.Locator.Local != "b.html#L8-L12" || c.Metadata["path"] != "b.html" || c.Metadata["anchor"] != "storage" {
		t.Fatalf("original mapping: %+v", c)
	}
	expanded, err := a.Expand(t.Context(), recall.ExpandRequest{Locator: c.Locator, Detail: recall.DetailFull})
	if err != nil || strings.Contains(expanded.Content, "<") || !strings.Contains(expanded.Content, "underground cistern") || !strings.Contains(expanded.Provenance, "L8-L12") {
		t.Fatalf("expand = %+v %v", expanded, err)
	}
	lexical := docs.New()
	defer lexical.Close()
	_, err = lexical.Initialize(t.Context(), adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, SourceID: "docs", Workdir: filepath.Join(filepath.Dir(cfg.Workdir), "docs"), Location: cfg.Location, Settings: map[string]any{"extensions": []string{".md", ".html"}, "sibling_preference": []string{".md", ".html"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lexical.Refresh(t.Context(), protocol.RefreshParams{}); err != nil {
		t.Fatal(err)
	}
	lr, err := lexical.Search(t.Context(), recall.SearchRequest{Query: "underground cistern"})
	if err != nil || len(lr.Candidates) != 1 {
		t.Fatalf("lexical = %+v %v", lr, err)
	}
	lc := lr.Candidates[0]
	if lc.SourceRecordID != c.SourceRecordID || lc.Title != c.Title {
		t.Fatalf("identity/title differ: %+v vs %+v", lc, c)
	}
	lc.SourceID = "docs"
	lc.SourceUID = "uid-docs"
	c.SourceID = "semantic"
	c.SourceUID = "uid-semantic"
	ranker, err := ranking.New(ranking.Config{Sources: map[recall.SourceUID]ranking.SourceConfig{"uid-docs": {BasePrior: 1}, "uid-semantic": {BasePrior: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	fused, err := ranker.Fuse(ranking.Request{Candidates: []recall.Candidate{lc, c}, Resolver: lineage.MapResolver{"docs": "uid-docs", "semantic": "uid-semantic"}, SourceLocations: map[recall.SourceUID]string{"uid-docs": cfg.Location, "uid-semantic": cfg.Location}})
	if err != nil || len(fused.Results) != 1 || len(fused.Results[0].Members) != 2 || fused.Results[0].Explanation.Corroboration.IndependentUnits != 1 {
		t.Fatalf("fusion = %+v %v", fused, err)
	}
}

func TestDerivedLifecycleAndSelection(t *testing.T) {
	a, e, cfg := derivedFixture(t, "vector")
	for rel, body := range map[string]string{"private.html": "<meta name=robots content=noindex><h1>Secret</h1>", "report.html": "<h1>Rendered</h1>", "report.md": "# Original", "page.template.html": "<h1>Template</h1>", "mockups/demo.html": "<h1>Mockup</h1>", ".private/page.html": "<h1>Dot directory</h1>", "nested/.git/HEAD": "ref: main", "nested/page.html": "<h1>Nested</h1>"} {
		writeDerived(t, cfg.Location, rel, body)
	}
	h := refreshMirror(t, a)
	if h.Status != recall.HealthHealthy || h.Diagnostics["mirrored_files"] != 1 || h.Diagnostics["excluded_files"] != 3 || h.Diagnostics["excluded_dirs"] != 3 {
		t.Fatalf("health = %+v", h)
	}
	file := filepath.Join(e.mirror, "b.html.md")
	stat, _ := os.Stat(file)
	// Artificially old mtime makes a rewrite visible without sleeps.
	old := time.Unix(1000, 0)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	refreshMirror(t, a)
	after, _ := os.Stat(file)
	if !after.ModTime().Equal(old) || after.Size() != stat.Size() {
		t.Fatal("unchanged mirror rewritten")
	}
	writeDerived(t, cfg.Location, "b.html", strings.ReplaceAll(reservoirHTML, "underground cistern", "buried tank"))
	before, _ := os.ReadFile(file)
	r, err := searchOnce(t, a, "below ground precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) != 0 {
		t.Fatalf("stale query = %+v %v", r, err)
	}
	now, _ := os.ReadFile(file)
	if string(before) != string(now) {
		t.Fatal("stale query changed mirror")
	}
	refreshMirror(t, a)
	c := requireMirrorHit(t, a)
	if !strings.Contains(c.Excerpt, "buried tank") {
		t.Fatalf("edit not refreshed: %+v", c)
	}
	writeDerived(t, cfg.Location, "b.md", "# Prefer this")
	if h = refreshMirror(t, a); h.Status != recall.HealthHealthy {
		t.Fatalf("sibling refresh = %+v", h)
	}
	if _, err = os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("newly shadowed mirror retained")
	}
	if len(e.indexed) != 0 {
		t.Fatal("shadowed embedding retained")
	}
	os.Remove(filepath.Join(cfg.Location, "b.md"))
	refreshMirror(t, a)
	os.Remove(filepath.Join(cfg.Location, "b.html"))
	if _, err = a.Expand(t.Context(), recall.ExpandRequest{Locator: c.Locator, Detail: recall.DetailFull}); !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("deleted expand = %v", err)
	}
	refreshMirror(t, a)
	if len(e.indexed) != 0 {
		t.Fatal("deleted embedding retained")
	}
	if _, err = os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted mirror retained")
	}
}

func TestDerivedFailedRefreshWrongPathAndRestart(t *testing.T) {
	a, e, cfg := derivedFixture(t, "vector")
	refreshMirror(t, a)
	c := requireMirrorHit(t, a)
	e.wrong = t.TempDir()
	calls := len(e.runner.invocations())
	h := refreshMirror(t, a)
	if h.Status != recall.HealthUnavailable {
		t.Fatalf("wrong path = %+v", h)
	}
	for _, args := range e.runner.invocations()[calls:] {
		if args[0] == "update" || args[0] == "embed" || (len(args) > 1 && args[1] == "add") {
			t.Fatalf("wrong-path maintenance = %v", args)
		}
	}
	r, err := searchOnce(t, a, "precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) > 0 {
		t.Fatalf("wrong-path search = %+v %v", r, err)
	}
	if _, err = a.Expand(t.Context(), recall.ExpandRequest{Locator: c.Locator}); err == nil {
		t.Fatal("wrong-path expansion accepted")
	}
	e.wrong = ""
	e.fail = "embed:html-view"
	h = refreshMirror(t, a)
	if h.Status != recall.HealthDegraded || h.Diagnostics["last_refresh_error"] == nil {
		t.Fatalf("failed embed = %+v", h)
	}
	restarted := qmd.New(qmd.Options{Runner: e.runner})
	defer restarted.Close()
	if _, err = restarted.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	calls = len(e.runner.invocations())
	r, err = searchOnce(t, restarted, "precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) > 0 {
		t.Fatalf("failed receipt survived = %+v %v", r, err)
	}
	for _, args := range e.runner.invocations()[calls:] {
		if args[0] == "embed" || args[0] == "update" {
			t.Fatalf("query repaired failed refresh: %v", args)
		}
	}
	e.fail = ""
	refreshMirror(t, restarted)
	requireMirrorHit(t, restarted)
	writeDerived(t, cfg.Location, "b.html", "<meta name=robots content=noindex>"+reservoirHTML)
	if _, err = restarted.Expand(t.Context(), recall.ExpandRequest{Locator: c.Locator}); !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("new noindex expand = %v", err)
	}
	r, err = searchOnce(t, restarted, "precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) > 0 {
		t.Fatalf("new noindex query = %+v %v", r, err)
	}
	refreshMirror(t, restarted)
	if len(e.indexed) != 0 {
		t.Fatal("new noindex embedding survived")
	}
}

func TestDerivedQueriesAllModesAndProtocol(t *testing.T) {
	for _, mode := range []string{"bm25", "vector", "hybrid", "full"} {
		t.Run(mode, func(t *testing.T) {
			a, e, _ := derivedFixture(t, mode)
			refreshMirror(t, a)
			e.primaryHit = true
			resp, err := searchOnce(t, a, "precipitation")
			if err != nil || resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 2 {
				t.Fatalf("merged collections = %+v %v", resp, err)
			}
			if resp.Candidates[0].SourceRecordID != "a.md" || resp.Candidates[1].SourceRecordID != "b.html" || resp.Candidates[1].LocalRank != 2 {
				t.Fatalf("merged order = %+v", resp.Candidates)
			}
		})
	}
	// Real protocol transport, schema validation and JSON encoding, without a
	// second parser or a recorded hand-written adapter response.
	a, e, cfg := derivedFixture(t, "vector")
	a.Close()
	a = qmd.New(qmd.Options{Runner: e.runner})
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- adapter.Serve(ctx, serverReader, serverWriter, a); serverWriter.Close() }()
	client, err := protocol.NewClient(clientReader, clientWriter, protocol.ClientOptions{Closer: clientWriter})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer clientReader.Close()
	defer serverReader.Close()
	var manifest recall.Manifest
	if err = client.Call(ctx, "recall/initialize", cfg, &manifest); err != nil {
		t.Fatal(err)
	}
	var health recall.Health
	if err = client.Call(ctx, "recall/refresh", protocol.RefreshParams{Deadline: time.Now().Add(time.Minute)}, &health); err != nil || health.Status != recall.HealthHealthy {
		t.Fatalf("protocol refresh = %+v %v", health, err)
	}
	var resp recall.SearchResponse
	if err = client.Call(ctx, "recall/search", recall.SearchRequest{Query: "below ground precipitation", Limit: 10, Deadline: time.Now().Add(time.Minute)}, &resp); err != nil || len(resp.Candidates) != 1 || resp.Candidates[0].SourceRecordID != "b.html" {
		t.Fatalf("protocol query = %+v %v", resp, err)
	}
	var expanded recall.ExpandResponse
	if err = client.Call(ctx, "recall/expand", recall.ExpandRequest{Locator: resp.Candidates[0].Locator, Detail: recall.DetailFull, Deadline: time.Now().Add(time.Minute)}, &expanded); err != nil || strings.Contains(expanded.Content, "<") {
		t.Fatalf("protocol expand = %+v %v", expanded, err)
	}
	client.Close()
	serverReader.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("protocol server did not stop")
	}
}

func TestDerivedDoesNotCiteOriginalChangedDuringSearch(t *testing.T) {
	for _, change := range []string{"delete", "noindex", "edit"} {
		t.Run(change, func(t *testing.T) {
			a, e, cfg := derivedFixture(t, "vector")
			refreshMirror(t, a)
			e.onSearch = func() {
				switch change {
				case "delete":
					os.Remove(filepath.Join(cfg.Location, "b.html"))
				case "noindex":
					writeDerived(t, cfg.Location, "b.html", "<meta name=robots content=noindex>"+reservoirHTML)
				case "edit":
					writeDerived(t, cfg.Location, "b.html", strings.ReplaceAll(reservoirHTML, "cistern", "pool"))
				}
			}
			r, err := searchOnce(t, a, "below ground precipitation")
			if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) != 0 {
				t.Fatalf("changed during search = %+v %v", r, err)
			}
		})
	}
}

func TestDerivedSelectionSettingsCleanupAndExtractionFailure(t *testing.T) {
	a, e, cfg := derivedFixture(t, "vector")
	refreshMirror(t, a)
	// Changing selection must not cite the old generation, including a restart.
	cfg.Settings["exclude_globs"] = []string{"b.html"}
	next := qmd.New(qmd.Options{Runner: e.runner})
	defer next.Close()
	if _, err := next.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r, err := searchOnce(t, next, "precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) != 0 {
		t.Fatalf("new exclusion cited = %+v %v", r, err)
	}
	h := refreshMirror(t, next)
	if h.Status != recall.HealthHealthy || len(e.indexed) != 0 {
		t.Fatalf("exclusion cleanup = %+v %v", h, e.indexed)
	}
	delete(cfg.Settings, "exclude_globs")
	cfg.Settings["max_file_bytes"] = 10
	capped := qmd.New(qmd.Options{Runner: e.runner})
	defer capped.Close()
	if _, err = capped.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	h = refreshMirror(t, capped)
	if h.Status != recall.HealthDegraded || h.Diagnostics["last_refresh_error"] == nil || len(e.indexed) != 0 {
		t.Fatalf("failed extraction = %+v", h)
	}
}

func TestDerivedRejectsAmbiguousVirtualPathsAndSymlinkArtifacts(t *testing.T) {
	a, e, cfg := derivedFixture(t, "vector")
	writeDerived(t, cfg.Location, "b-html.html", "<h1>One</h1>")
	writeDerived(t, cfg.Location, "b.html.html", "<h1>Other</h1>")
	h := refreshMirror(t, a)
	if h.Status != recall.HealthDegraded || h.FailedCount != 2 {
		t.Fatalf("virtual collision health = %+v", h)
	}
	for rel := range e.indexed {
		if rel != "b.html.md" {
			t.Fatalf("colliding mirror indexed: %s", rel)
		}
	}
	os.Remove(filepath.Join(cfg.Location, "b-html.html"))
	os.Remove(filepath.Join(cfg.Location, "b.html.html"))
	refreshMirror(t, a)
	external := t.TempDir()
	os.Symlink(external, filepath.Join(e.mirror, "escaped"))
	h = refreshMirror(t, a)
	if h.Status != recall.HealthUnavailable {
		t.Fatalf("mirror symlink accepted: %+v", h)
	}
	// The mirror does not exist yet. Its existing ancestor points into the
	// corpus; reject before creating any corpus directories or collection.
	parent := t.TempDir()
	os.Symlink(cfg.Location, filepath.Join(parent, "state"))
	cfg.Workdir = filepath.Join(parent, "state", "adapters", "uid")
	other := qmd.New(qmd.Options{Runner: e.runner})
	defer other.Close()
	if _, err := other.Initialize(t.Context(), cfg); err == nil {
		t.Fatal("missing mirror path under symlink into corpus accepted")
	}
	if _, err := os.Stat(filepath.Join(cfg.Location, "qmd-derived")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("handshake wrote into corpus")
	}
}

func TestDerivedSettingsValidation(t *testing.T) {
	for _, settings := range []map[string]any{
		{"derive_extensions": []string{".pdf"}, "derived_collection": "html"},
		{"derive_extensions": []string{".html"}},
		{"derive_extensions": []string{".html"}, "derived_collection": "fixture"},
		{"derive_extensions": []string{".html"}, "derived_collection": "../html"},
		{"derive_extensions": []any{3}, "derived_collection": "html"},
		{"exclude_dirs": []string{"a/b"}}, {"exclude_dirs": []string{"["}},
		{"exclude_globs": []string{"../private"}}, {"max_raw_bytes": 0},
		{"sibling_preference": []string{"html"}},
	} {
		t.Run(fmt.Sprint(settings), func(t *testing.T) {
			settings["collection"] = "fixture"
			a := qmd.New(qmd.Options{})
			defer a.Close()
			_, err := a.Initialize(t.Context(), adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(), SourceID: "s", Location: t.TempDir(), Settings: settings})
			if !errors.Is(err, protocol.ErrInvalidParams) {
				t.Fatalf("invalid settings accepted: %v", err)
			}
		})
	}
}

func TestDerivedInvalidatesReceiptBeforeCollectionCreation(t *testing.T) {
	a, engine, cfg := derivedFixture(t, "vector")
	refreshMirror(t, a)
	// A previously successful collection was removed externally. Re-creating
	// it may itself index the mirror, so its old ready receipt must be revoked.
	engine.exists = false
	previous := engine.runner.fallback
	sawAdd := false
	engine.runner.fallback = func(args []string) (qmd.Result, error) {
		if len(args) > 1 && args[0] == "collection" && args[1] == "add" {
			sawAdd = true
			raw, err := os.ReadFile(filepath.Join(cfg.Workdir, "qmd-derived.json"))
			if err != nil {
				t.Fatal(err)
			}
			var r struct {
				Ready bool `json:"ready"`
			}
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			if r.Ready {
				t.Fatal("collection creation started with an old ready receipt")
			}
			return qmd.Result{ExitCode: 1, Stderr: []byte("collection creation interrupted")}, nil
		}
		return previous(args)
	}
	if h := refreshMirror(t, a); h.Status == recall.HealthHealthy || !sawAdd {
		t.Fatalf("failed collection recreation: %+v, add=%v", h, sawAdd)
	}
	if r, err := searchOnce(t, a, "below ground precipitation"); len(r.Candidates) > 0 {
		t.Fatalf("incomplete recreated collection served: %+v %v", r, err)
	}
}

func TestSharedDerivedDataNoIndexRefresh(t *testing.T) {
	initial, _, cfg := derivedFixture(t, "vector")
	initial.Close()
	cfg.SharedWorkdir = filepath.Join(t.TempDir(), "sources", "uid")
	e := newMirrorEngine(cfg.Location, filepath.Join(cfg.SharedWorkdir, "qmd-derived"))
	a := qmd.New(qmd.Options{Runner: e.runner})
	defer a.Close()
	if _, err := a.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	marked := strings.Replace(reservoirHTML, "\n\n\n", "\n<div data-noindex=\"false\"><h1>Mockheading</h1><p>mockbody</p><img alt=\"mockalt\"><ul><li>mocklist</li></ul><table><tr><td>mockcell</td></tr></table>\n</div>\n", 1)
	visible := strings.Replace(marked, ` data-noindex="false"`, "", 1)
	writeDerived(t, cfg.Location, "b.html", visible)
	refreshMirror(t, a)
	for _, body := range []string{marked, visible, marked} {
		writeDerived(t, cfg.Location, "b.html", body)
		// A second profile shares the receipt and mirror, but cannot cite the
		// previous embedding or silently refresh it when the source changes.
		otherCfg := cfg
		otherCfg.Workdir = t.TempDir()
		other := qmd.New(qmd.Options{Runner: e.runner})
		if _, err := other.Initialize(t.Context(), otherCfg); err != nil {
			t.Fatal(err)
		}
		calls := len(e.runner.invocations())
		r, err := searchOnce(t, other, "precipitation")
		if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) != 0 {
			t.Fatalf("stale shared mirror: %+v, %v", r, err)
		}
		for _, args := range e.runner.invocations()[calls:] {
			if args[0] == "update" || args[0] == "embed" {
				t.Fatalf("query refreshed mirror: %v", args)
			}
		}
		if h := refreshMirror(t, other); h.Status != recall.HealthHealthy {
			t.Fatalf("refresh: %+v", h)
		}
		raw, err := os.ReadFile(filepath.Join(e.mirror, "b.html.md"))
		if err != nil {
			t.Fatal(err)
		}
		hidden := body == marked
		for _, text := range []string{string(raw), e.indexed["b.html.md"]} {
			for _, term := range []string{"Mockheading", "mockbody", "mockalt", "mocklist", "mockcell"} {
				if strings.Contains(text, term) == hidden {
					t.Fatalf("hidden=%v, term %s in mirror/index: %s", hidden, term, text)
				}
			}
			if !strings.Contains(text, "underground cistern") {
				t.Fatalf("outside evidence lost: %s", text)
			}
		}
		if hidden {
			c := requireMirrorHit(t, a)
			if c.Locator.Local != "b.html#L8-L12" || c.Title != "Rainwater reservoir" {
				t.Fatalf("outside mapping changed: %+v", c)
			}
			expanded, err := other.Expand(t.Context(), recall.ExpandRequest{Locator: c.Locator, Detail: recall.DetailFull})
			if err != nil || strings.Contains(expanded.Content, "mock") || !strings.Contains(expanded.Content, "underground cistern") {
				t.Fatalf("expand: %+v, %v", expanded, err)
			}
		}
		other.Close()
	}

	// Simulate the v0.7.0 settings digest (before extraction versioning), with
	// identical file bytes and mirror hashes. Even an unchanged extraction
	// must be re-embedded explicitly under the new extraction contract.
	receiptPath := filepath.Join(cfg.SharedWorkdir, "qmd-derived.json")
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	legacyValues := []any{[]string{".html"}, "html-view", document.Selection{ExcludeDirs: []string{".*", "node_modules"}, ExcludeNestedRepos: true, ExcludeGlobs: []string{"**/mockups/**", "*.template.html"}, SiblingPreference: []string{".md", ".html"}}, int64(16 << 20), int64(1 << 20)}
	legacySettings, err := json.Marshal(legacyValues)
	if err != nil {
		t.Fatal(err)
	}
	current, _, ok := strings.Cut(receipt["config"].(string), " derived=")
	if !ok {
		t.Fatal("missing derived settings digest")
	}
	versioned, err := json.Marshal(append(legacyValues, document.HTMLExtractionVersion))
	if err != nil || receipt["config"] != fmt.Sprintf("%s derived=%x", current, sha256.Sum256(versioned)) {
		t.Fatalf("legacy settings fixture does not match current settings: %v", err)
	}
	receipt["config"] = fmt.Sprintf("%s derived=%x", current, sha256.Sum256(legacySettings))
	raw, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := qmd.New(qmd.Options{Runner: e.runner})
	defer restarted.Close()
	if _, err := restarted.Initialize(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	r, err := searchOnce(t, restarted, "precipitation")
	if err != nil || r.Outcome != recall.SearchPartial || len(r.Candidates) != 0 {
		t.Fatalf("old extraction receipt served: %+v, %v", r, err)
	}
	if h := refreshMirror(t, restarted); h.Status != recall.HealthHealthy {
		t.Fatalf("upgrade refresh: %+v", h)
	}
	requireMirrorHit(t, restarted)
}

func TestDerivedQMD28PreservedFilenameMapsToOriginal(t *testing.T) {
	a, engine, _ := derivedFixture(t, "vector")
	engine.preservePaths = true
	if health := refreshMirror(t, a); health.Status != recall.HealthHealthy {
		t.Fatalf("refresh: %+v", health)
	}
	hit := requireMirrorHit(t, a)
	if hit.SourceRecordID != "b.html" || strings.Contains(hit.Locator.Local, ".html.md") {
		t.Fatalf("physical mirror path leaked: %+v", hit)
	}
}
