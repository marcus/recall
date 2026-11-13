package qmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Opt-in upstream SDK contract proof: real qmd SQLite/chunking, mocked network
// inference. No local model, inference service, download or home index is used.
func TestHTTPInferenceQMDJourney(t *testing.T) {
	packagePath := os.Getenv("RECALL_QMD_HTTP_PACKAGE")
	if packagePath == "" {
		t.Skip("set RECALL_QMD_HTTP_PACKAGE to an installed qmd package for the upstream SDK proof")
	}
	var calls atomic.Int32
	var fail atomic.Bool
	var pauseAt atomic.Int32
	var track atomic.Bool
	var completedInputs sync.Map
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "private error text", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Model   string   `json:"model"`
			Content string   `json:"content"`
			Input   []string `json:"input"`
			Tokens  []int    `json:"tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{"model": body.Model}
		switch r.URL.Path {
		case "/v1/tokenize":
			tokens := []int{}
			for _, c := range body.Content {
				tokens = append(tokens, int(c))
			}
			result["tokens"] = tokens
		case "/v1/detokenize":
			var text strings.Builder
			for _, c := range body.Tokens {
				text.WriteRune(rune(c))
			}
			result["content"] = text.String()
		case "/v1/embeddings":
			if pauseAt.Load() > 0 && pauseAt.Add(-1) == 0 {
				close(blocked)
				<-r.Context().Done()
				return
			}
			data := []any{}
			for i, text := range body.Input {
				if !strings.HasPrefix(text, "title: ") && !strings.HasPrefix(text, "task: search result | query: ") {
					t.Errorf("unformatted inference text")
				}
				if track.Load() && strings.HasPrefix(text, "title: ") {
					if _, exists := completedInputs.LoadOrStore(text, true); exists {
						t.Error("completed partial-document chunk repeated inference after restart")
					}
				}
				vector := []float64{1, 0, 0}
				if strings.Contains(text, "bicycle") {
					vector = []float64{0, 1, 0}
				}
				data = append(data, map[string]any{"index": i, "embedding": vector})
			}
			result["data"] = data
		default:
			t.Errorf("hidden inference request: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".qmd"), 0700); err != nil {
		t.Fatal(err)
	}
	config := `collections:
  notes:
    path: ` + root + `
    pattern: "**/*.md"
`
	if err := os.WriteFile(filepath.Join(root, ".qmd", "index.yml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"reservoir.md": "# Reservoir\nRainwater accumulates in an underground cistern.\n", "bicycle.md": "# bicycle\nKeep bicycle tyres inflated.\n", "page.html": "<h1>Reservoir</h1><p>Water collects in a stone cistern.</p>"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	settings := inferenceSettings()
	settings["inference_qmd_package"] = packagePath
	settings["inference_index"] = filepath.Join(t.TempDir(), "provider.sqlite")
	settings["inference_url"] = server.URL
	settings["inference_dimensions"] = 3
	cfg := adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, SourceID: "semantic", Location: root, Workdir: t.TempDir(), SharedWorkdir: t.TempDir(), Settings: settings}
	a := New(Options{LockDir: t.TempDir()})
	defer a.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := a.Initialize(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	health, err := a.Health(ctx)
	if err != nil || health.Status == recall.HealthHealthy {
		t.Fatalf("missing index: %+v %v", health, err)
	}
	if _, err := os.Stat(settings["inference_index"].(string)); !os.IsNotExist(err) {
		t.Fatal("health created an index")
	}
	health, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || health.Status != recall.HealthHealthy {
		t.Fatalf("refresh: %+v %v", health, err)
	}
	response, err := a.Search(ctx, recall.SearchRequest{Query: "precipitation", Limit: 5})
	if err != nil || response.Outcome != recall.SearchSuccess || len(response.Candidates) != 1 {
		t.Fatalf("query: %+v %v", response, err)
	}
	if response.Candidates[0].SourceRecordID != "reservoir.md" {
		t.Fatalf("wrong result: %+v", response.Candidates)
	}
	evidence, err := a.Expand(ctx, recall.ExpandRequest{Locator: response.Candidates[0].Locator, Detail: recall.DetailFull})
	if err != nil || !strings.Contains(evidence.Content, "underground cistern") {
		t.Fatalf("expand: %+v %v", evidence, err)
	}
	fail.Store(true)
	response, err = a.Search(ctx, recall.SearchRequest{Query: "precipitation", Limit: 5})
	if err == nil && response.Outcome == recall.SearchSuccess {
		t.Fatalf("unavailable provider became successful search: %+v", response)
	}
	fail.Store(false)
	before := calls.Load()
	settings["inference_model"] = "another-immutable-model"
	other := New(Options{LockDir: t.TempDir()})
	defer other.Close()
	if _, err := other.Initialize(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	health, err = other.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || health.Status != recall.HealthUnavailable {
		t.Fatalf("model mismatch refresh: %+v %v", health, err)
	}
	if calls.Load() != before {
		t.Fatal("model mismatch reached inference")
	}
	settings["inference_model"] = "embeddinggemma-300m-q8-0-sha256"
	settings["derive_extensions"] = []string{".html"}
	settings["derived_collection"] = "html"
	derived := New(Options{LockDir: t.TempDir()})
	defer derived.Close()
	if _, err := derived.Initialize(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	health, err = derived.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || health.Status != recall.HealthHealthy {
		t.Fatalf("derived refresh: %+v %v", health, err)
	}
	response, err = derived.Search(ctx, recall.SearchRequest{Query: "precipitation", Limit: 5})
	if err != nil || response.Outcome != recall.SearchSuccess {
		t.Fatalf("derived query: %+v %v", response, err)
	}
	found := false
	for _, c := range response.Candidates {
		if c.SourceRecordID == "page.html" {
			found = true
		}
	}
	if !found {
		t.Fatalf("derived original missing: %+v", response)
	}
	// Interrupt a real qmd document after its first 32-chunk batch, then prove
	// the next process reuses those exact inference results from qmd's cache.
	var large strings.Builder
	large.WriteString("# Large document\n")
	for i := 0; i < 1800; i++ {
		fmt.Fprintf(&large, "Unique evidence line %d contains water storage information.\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "large.md"), []byte(large.String()), 0600); err != nil {
		t.Fatal(err)
	}
	track.Store(true)
	pauseAt.Store(3)
	interrupted, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := a.Refresh(interrupted, protocol.RefreshParams{}); done <- err }()
	select {
	case <-blocked:
		stop()
	case <-ctx.Done():
		stop()
		t.Fatal("partial document did not reach the controlled interruption")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted refresh: %v", err)
	}
	pauseAt.Store(0)
	health, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || health.Status != recall.HealthHealthy {
		t.Fatalf("resumed document: %+v %v", health, err)
	}

}
