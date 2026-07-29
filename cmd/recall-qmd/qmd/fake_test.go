package qmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// fakeRunner answers invocations from a table keyed on the first argument, so a
// test states the qmd output it wants without needing qmd, models, or an index.
//
// It is a test seam and not the determinism story: a committed evaluation pack
// configures [qmd.ReplayRunner] through the `replay` settings key, because a
// pack is configuration and cannot inject a Go value. Both are exercised here.
type fakeRunner struct {
	mu sync.Mutex

	// byKey maps the first argv token to a canned result.
	byKey map[string]qmd.Result
	// err is returned for any invocation whose key is missing, which is how a
	// test asserts that nothing unrecorded was spawned.
	fallback func(args []string) (qmd.Result, error)

	calls [][]string
	now   time.Time
	root  string
	delay time.Duration
}

func (f *fakeRunner) Kind() string { return "live" }

func (f *fakeRunner) Now() (time.Time, bool) { return f.now, !f.now.IsZero() }

func (f *fakeRunner) Root() (string, bool) { return f.root, f.root != "" }

func (f *fakeRunner) Run(ctx context.Context, args ...string) (qmd.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return qmd.Result{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return qmd.Result{}, err
	}
	if got, ok := f.byKey[args[0]]; ok {
		return got, nil
	}
	if f.fallback != nil {
		return f.fallback(args)
	}
	return qmd.Result{ExitCode: 1, Stderr: []byte("unrecorded")}, nil
}

func (f *fakeRunner) invocations() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.calls...)
}

func (f *fakeRunner) ran(sub string) bool {
	for _, args := range f.invocations() {
		if len(args) > 0 && args[0] == sub {
			return true
		}
	}
	return false
}

const fixtureClock = "2026-07-29T12:00:00Z"

// statusText is `qmd status` for a collection of n documents and v vectors.
func statusText(root, collection string, documents, vectors, files int) string {
	return strings.Join([]string{
		"QMD Status",
		"",
		"Index: " + filepath.Join(root, ".qmd", "index.sqlite"),
		"Size:  3.1 MB",
		"",
		"Documents",
		"  Total:    " + itoa(documents) + " files indexed",
		"  Vectors:  " + itoa(vectors) + " embedded",
		"  Updated:  3s ago",
		"",
		"Collections",
		"  " + collection + " (qmd://" + collection + "/)",
		"    Pattern:  **/*.md",
		"    Files:    " + itoa(files) + " (updated 3s ago)",
		"",
		"Models",
		"  Embedding:   https://huggingface.co/ggml-org/embeddinggemma-300M-GGUF",
		"  Reranking:   https://huggingface.co/ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF",
		"  Generation:  https://huggingface.co/tobil/qmd-query-expansion-1.7B-gguf",
		"",
	}, "\n")
}

func collectionText(root, collection string) string {
	return strings.Join([]string{
		"Collection: " + collection,
		"  Path:     " + root,
		"  Pattern:  **/*.md",
		"  Include:  yes (default)",
		"",
	}, "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// corpus writes a small Markdown tree and returns its root.
func corpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes/tooth-care.md", strings.Join([]string{
		"# Tooth care appointment",
		"",
		"## Question",
		"",
		"Find a dental hygienist who takes the Sample Dental plan.",
		"",
		"## Recommendation",
		"",
		"Riverside Family Dental had openings.",
	}, "\n"))
	write("guides/sourdough.md", "# Sourdough starter\n\nEqual parts flour and water.\n")
	return root
}

// hit is one recorded qmd result, as JSON.
func hit(docid, file, title string, line, start, count int, score float64, explain string) string {
	snippet := "@@ -" + itoa(start) + "," + itoa(count) + " @@ (0 before, 4 after)\\n" +
		"Find a dental hygienist who takes the Sample Dental plan.\\n"
	body := `{"docid":"#` + docid + `","score":` + ftoa(score) +
		`,"file":"` + file + `","line":` + itoa(line) +
		`,"title":"` + title + `","snippet":"` + snippet + `"`
	if explain != "" {
		body += `,"explain":` + explain
	}
	return body + "}"
}

func ftoa(f float64) string {
	// Small fixed set of scores in these tests; formatting them by hand keeps
	// the recorded JSON readable in the assertions.
	switch f {
	case 1:
		return "1"
	case 0.5:
		return "0.5"
	case 0.25:
		return "0.25"
	default:
		return "0.9"
	}
}

const lexicalExplain = `{"ftsScores":[2.5],"vectorScores":[],"rrf":{"rank":1,` +
	`"positionScore":1,"weight":1,"baseScore":0.05,"topRankBonus":0.05,"totalScore":0.1,` +
	`"contributions":[{"listIndex":0,"source":"fts","queryType":"original",` +
	`"query":"dental hygienist","rank":1,"weight":2,"backendScore":2.5,"rrfContribution":0.03}]},` +
	`"rerankScore":0,"blendedScore":0}`

const semanticExplain = `{"ftsScores":[],"vectorScores":[0.48],"rrf":{"rank":1,` +
	`"positionScore":1,"weight":1,"baseScore":0.05,"topRankBonus":0.05,"totalScore":0.1,` +
	`"contributions":[{"listIndex":0,"source":"vec","queryType":"hyde",` +
	`"query":"a hypothetical answer about dental hygiene","rank":1,"weight":1,` +
	`"backendScore":0.48,"rrfContribution":0.016}]},"rerankScore":0.62,"blendedScore":0.87}`

// newAdapter hands back a handshaken adapter over a fake runner.
func newAdapter(t *testing.T, root string, settings map[string]any, runner *fakeRunner) *qmd.Adapter {
	t.Helper()
	if runner.now.IsZero() {
		runner.now = mustTime(t, fixtureClock)
	}
	a := qmd.New(qmd.Options{Runner: runner})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1,
		ProtocolVersionMax: 1,
		Workdir:            t.TempDir(),
		SourceID:           "qmd",
		Location:           root,
		Settings:           settings,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func healthyRunner(root string, results string) *fakeRunner {
	return &fakeRunner{
		byKey: map[string]qmd.Result{
			"--version":  {Stdout: []byte("qmd 2.5.3\n")},
			"collection": {Stdout: []byte(collectionText(root, "fixture"))},
			"status":     {Stdout: []byte(statusText(root, "fixture", 2, 2, 2))},
			"query":      {Stdout: []byte(results)},
			"search":     {Stdout: []byte(results)},
			"vsearch":    {Stdout: []byte(results)},
			"update":     {Stdout: []byte("✓ All collections updated.\n")},
			"embed":      {Stdout: []byte("✓ Done! Embedded 2 chunks from 2 documents in 1s\n")},
		},
	}
}

func baseSettings() map[string]any {
	return map[string]any{"collection": "fixture"}
}

func mustTime(t *testing.T, text string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatal(err)
	}
	return got.UTC()
}

func searchOnce(t *testing.T, a *qmd.Adapter, query string) (recall.SearchResponse, error) {
	t.Helper()
	return a.Search(context.Background(), recall.SearchRequest{
		Query:    query,
		Limit:    10,
		Deadline: time.Now().Add(time.Minute),
	})
}

func TestHandshakeDeclaresPerModeCapability(t *testing.T) {
	root := corpus(t)
	for _, tc := range []struct {
		mode  string
		modes []recall.QueryMode
	}{
		{"bm25", []recall.QueryMode{recall.QueryLexical}},
		{"vector", []recall.QueryMode{recall.QuerySemantic}},
		{"hybrid", []recall.QueryMode{recall.QueryLexical, recall.QuerySemantic}},
		{"full", []recall.QueryMode{recall.QueryLexical, recall.QuerySemantic}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			settings := baseSettings()
			settings["mode"] = tc.mode
			a := newAdapter(t, root, settings, healthyRunner(root, "[]"))
			manifest, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: 1, ProtocolVersionMax: 1,
				Workdir: t.TempDir(), SourceID: "qmd", Location: root, Settings: settings,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.QueryModes) != len(tc.modes) {
				t.Fatalf("mode %s declared %v, want %v", tc.mode, manifest.QueryModes, tc.modes)
			}
			for i, want := range tc.modes {
				if manifest.QueryModes[i] != want {
					t.Fatalf("mode %s declared %v, want %v", tc.mode, manifest.QueryModes, tc.modes)
				}
			}
			if manifest.AsOfSupport != recall.AsOfNone {
				t.Fatalf("as_of_support = %q, want none", manifest.AsOfSupport)
			}
			if manifest.Sensitivity != recall.SensitivityInternal {
				t.Fatalf("sensitivity = %v, want internal", manifest.Sensitivity)
			}
		})
	}
}

// The handshake must not spawn qmd. A missing binary has to be reportable as an
// unavailable source with a reason; a handshake that failed on it would leave
// the source unable to report anything at all, and a first refresh downloads
// about 2GB inside the core's ten-second handshake timeout.
func TestHandshakeSpawnsNothing(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	newAdapter(t, root, baseSettings(), runner)
	if got := runner.invocations(); len(got) != 0 {
		t.Fatalf("handshake spawned %v", got)
	}
}

func TestSearchFillsTheEnvelope(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results))

	resp, err := searchOnce(t, a, "dental hygienist plan")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %q, want success: %v", resp.Outcome, resp.Diagnostics)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(resp.Candidates))
	}
	c := resp.Candidates[0]
	if c.SourceRecordID != "notes/tooth-care.md" {
		t.Errorf("source_record_id = %q, want the collection-relative path", c.SourceRecordID)
	}
	if c.Locator.Local != "notes/tooth-care.md#L5-L6" {
		t.Errorf("locator local = %q, want notes/tooth-care.md#L5-L6", c.Locator.Local)
	}
	if c.CandidateID == c.SourceRecordID {
		t.Error("candidate_id must identify the hit, not the record")
	}
	if c.ExcerptKind != recall.ExcerptMatched {
		t.Errorf("excerpt_kind = %q, want matched", c.ExcerptKind)
	}
	if c.ContentFingerprint != "qmd-docid:43f92c" {
		t.Errorf("content_fingerprint = %q", c.ContentFingerprint)
	}
	if c.LocalScore == nil || *c.LocalScore != 1 {
		t.Errorf("local_score = %v, want qmd's own score", c.LocalScore)
	}
	if c.EventTime != nil {
		t.Error("event_time must be omitted: a file mtime is a property of the checkout")
	}
	if c.ConfirmedAt == nil {
		t.Error("a complete boundary confirms its candidates")
	}
}

// Relevance is the one number compared across sources, and a nil one is read as
// 1.0 — the maximum. A source that omitted it would outrank every source that
// reports honestly, so it is never omitted here, including for a result whose
// text carries no query term at all.
func TestRelevanceIsNeverNil(t *testing.T) {
	root := corpus(t)
	results := "[" +
		hit("43f92c", "qmd://fixture/notes/tooth-care.md", "Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "," +
		hit("0593a8", "qmd://fixture/guides/sourdough.md", "Sourdough starter", 1, 1, 2, 0.5, semanticExplain) +
		"]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results))

	resp, err := searchOnce(t, a, "dental hygienist plan")
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range resp.Candidates {
		if c.Relevance == nil {
			t.Fatalf("candidate %d omitted relevance", i)
		}
		if *c.Relevance < 0 || *c.Relevance > 1 {
			t.Fatalf("candidate %d relevance %v out of range", i, *c.Relevance)
		}
	}
	if *resp.Candidates[0].Relevance == 0 {
		t.Fatal("a hit whose text carries the query terms must measure above zero")
	}
	// qmd's own number must not reach the field. The second result scored 0.5
	// against the first's 1.0, and both replay the same span text, so a relevance
	// that tracked the score would order them apart.
	for i, c := range resp.Candidates {
		if c.LocalScore == nil {
			t.Fatalf("candidate %d omitted qmd's score", i)
		}
		if *c.Relevance == *c.LocalScore {
			t.Fatalf("candidate %d relevance equals qmd's score (%v)", i, *c.Relevance)
		}
	}
}

// A semantic hit sharing no term with the query measures zero, and that is the
// honest number under the one shared definition — relevance is lexical
// coverage times concentration, and a paraphrase covers nothing. It is recorded
// as a test because it is the integration's central limitation, not an accident:
// see the note in doc.go.
func TestParaphraseHitMeasuresZeroRelevance(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results))

	resp, err := searchOnce(t, a, "who can clean my teeth")
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Candidates[0]
	if c.Relevance == nil || *c.Relevance != 0 {
		t.Fatalf("relevance = %v, want 0 for a hit sharing no query term", c.Relevance)
	}
	if !c.HasSignal(recall.MatchSemantic) {
		t.Error("a vector-only hit must carry the semantic signal")
	}
}

// Signals come from the score trace rather than from the mode, because the mode
// says which backends could have contributed and the trace says which did.
func TestMatchSignalsFollowTheTrace(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, lexicalExplain) + "]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results))

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Candidates[0]
	if !c.HasSignal(recall.MatchLexical) {
		t.Error("an FTS-only hit must carry the lexical signal")
	}
	if c.HasSignal(recall.MatchSemantic) {
		t.Error("an FTS-only hit must not claim a semantic match")
	}
	if c.HasSignal(recall.MatchExactIdentifier) {
		t.Error("this adapter has no identifier lookup and must never promote one")
	}
}

// Per-result attribution: the layered pipeline is only reviewable if each
// candidate carries which list produced it, at what rank and weight.
func TestPerResultComponentSignals(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "]"
	settings := baseSettings()
	settings["mode"] = "full"
	a := newAdapter(t, root, settings, healthyRunner(root, results))

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	signals, ok := resp.Candidates[0].Metadata["signals"].(map[string]any)
	if !ok {
		t.Fatalf("metadata carries no signals: %v", resp.Candidates[0].Metadata)
	}
	for _, key := range []string{"rrf_rank", "rrf_score", "rerank_score", "blended_score", "components"} {
		if _, ok := signals[key]; !ok {
			t.Errorf("signals missing %q: %v", key, signals)
		}
	}
	components, ok := signals["components"].([]map[string]any)
	if !ok || len(components) == 0 {
		t.Fatalf("components = %v", signals["components"])
	}
	if components[0]["source"] != "vec" {
		t.Errorf("component source = %v, want vec", components[0]["source"])
	}
	if _, ok := resp.Diagnostics["expanded_queries"]; !ok {
		t.Error("the expanded queries are the evidence that expansion fired")
	}
}

// A mode that ran no reranker must not report a rerank score. qmd writes zero
// when it did not rerank, and a zero would read as "scored at nothing".
func TestRerankScoreOnlyWhenItRan(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results)) // hybrid

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	signals := resp.Candidates[0].Metadata["signals"].(map[string]any)
	if _, ok := signals["rerank_score"]; ok {
		t.Error("hybrid mode reports a rerank score it did not compute")
	}
	if resp.Diagnostics["rerank"] != false {
		t.Errorf("diagnostics claim rerank = %v in hybrid mode", resp.Diagnostics["rerank"])
	}
}

func TestModeChoosesTheCommandLine(t *testing.T) {
	root := corpus(t)
	for mode, want := range map[string][]string{
		"bm25":   {"search", "--json", "-n", "10", "-c", "fixture", "--", "dental"},
		"vector": {"vsearch", "--json", "-n", "10", "-c", "fixture", "--", "dental"},
		"hybrid": {"query", "--json", "--explain", "--no-rerank", "-n", "10", "-c", "fixture", "--", "dental"},
		"full":   {"query", "--json", "--explain", "-n", "10", "-c", "fixture", "--", "dental"},
	} {
		t.Run(mode, func(t *testing.T) {
			settings := baseSettings()
			settings["mode"] = mode
			runner := healthyRunner(root, "[]")
			a := newAdapter(t, root, settings, runner)
			if _, err := searchOnce(t, a, "dental"); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, args := range runner.invocations() {
				if args[0] == "search" || args[0] == "vsearch" || args[0] == "query" {
					got = args
				}
			}
			if strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("argv = %v, want %v", got, want)
			}
		})
	}
}

// The mode is part of index_config so that two runs under different modes are
// never compared by accident, and the model identities are there so a package
// bump cannot silently change ranking.
func TestIndexConfigNamesModeAndModels(t *testing.T) {
	root := corpus(t)
	for mode, wants := range map[string][]string{
		"bm25":   {"mode=bm25", "qmd=2.5.3"},
		"hybrid": {"mode=hybrid", "embed=embeddinggemma-300M-GGUF", "expand=qmd-query-expansion-1.7B-gguf"},
		"full":   {"mode=full", "rerank=Qwen3-Reranker-0.6B-Q8_0-GGUF"},
	} {
		t.Run(mode, func(t *testing.T) {
			settings := baseSettings()
			settings["mode"] = mode
			a := newAdapter(t, root, settings, healthyRunner(root, "[]"))
			health, err := a.Health(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range wants {
				if !strings.Contains(health.IndexConfig, want) {
					t.Errorf("index_config %q lacks %q", health.IndexConfig, want)
				}
			}
			if mode == "bm25" {
				if health.IndexModel != "" {
					t.Errorf("bm25 named an embedding model it never used: %q", health.IndexModel)
				}
				if strings.Contains(health.IndexConfig, "embed=") {
					t.Errorf("bm25 index_config names an embedding model: %q", health.IndexConfig)
				}
			}
		})
	}
}

// One store, one claim. Two instances over one index and one collection must
// report the same identity so `recall doctor` can refuse the profile; two over
// different collections of one index are two corpora and must not.
func TestStoreIdentityNamesIndexAndCollection(t *testing.T) {
	root := corpus(t)
	identity := func(collection string) string {
		settings := baseSettings()
		settings["collection"] = collection
		runner := &fakeRunner{byKey: map[string]qmd.Result{
			"--version":  {Stdout: []byte("qmd 2.5.3\n")},
			"collection": {Stdout: []byte(collectionText(root, collection))},
			"status":     {Stdout: []byte(statusText(root, collection, 2, 2, 2))},
		}}
		a := newAdapter(t, root, settings, runner)
		health, err := a.Health(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := health.Diagnostics[protocol.DiagStoreIdentity].(string)
		return got
	}
	first, second, other := identity("fixture"), identity("fixture"), identity("elsewhere")
	switch {
	case first == "":
		t.Fatal("a live source published no store identity")
	case first != second:
		t.Fatal("one index and one collection reported two identities")
	case first == other:
		t.Fatal("two collections of one index reported one identity")
	case strings.Contains(first, root):
		t.Fatalf("store identity carries a path: %q", first)
	}
}

// A replaying source opened nothing, so it claims no store. A value derived
// from a fixture directory would make two packs in one profile look like two
// sources over one index.
func TestReplayPublishesNoStoreIdentity(t *testing.T) {
	dir := replayPack(t, "[]")
	settings := baseSettings()
	settings["replay"] = dir
	a := qmd.New(qmd.Options{})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "qmd",
		Location: filepath.Join(dir, qmd.ReplayCorpusDir), Settings: settings,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := health.Diagnostics[protocol.DiagStoreIdentity]; ok {
		t.Fatal("a replaying source claimed a store")
	}
	if health.Diagnostics["transport"] != "replay" {
		t.Fatalf("transport = %v", health.Diagnostics["transport"])
	}
}

func TestRefreshRunsMaintenanceThenWarms(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	a := newAdapter(t, root, baseSettings(), runner)

	health, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthHealthy {
		t.Fatalf("status = %q: %v", health.Status, health.Diagnostics)
	}
	for _, sub := range []string{"update", "embed", "query"} {
		if !runner.ran(sub) {
			t.Errorf("refresh did not run %q: %v", sub, runner.invocations())
		}
	}
}

// bm25 consults no model, so a refresh in that mode must not download 2GB to
// serve a full-text search.
func TestRefreshSkipsModelsForBM25(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	settings := baseSettings()
	settings["mode"] = "bm25"
	a := newAdapter(t, root, settings, runner)

	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		t.Fatal(err)
	}
	if runner.ran("embed") {
		t.Error("bm25 refresh embedded the corpus")
	}
	if !runner.ran("update") {
		t.Error("bm25 refresh did not reindex")
	}
}

// A search must never be able to rebuild an index as a side effect. The argv
// allowlist is what enforces it, and this is the assertion that the search path
// cannot reach a maintenance shape.
func TestSearchNeverRunsMaintenance(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	a := newAdapter(t, root, baseSettings(), runner)
	if _, err := searchOnce(t, a, "dental"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"update", "embed"} {
		if runner.ran(sub) {
			t.Fatalf("a query reached %q", sub)
		}
	}
}
