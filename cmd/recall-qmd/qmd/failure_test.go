package qmd_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Every test in this file asserts the same invariant from a different failure:
// a missing dependency, an unreachable index, a broken contract, a wrong corpus,
// or an expired deadline is never reported as a successful empty result. An
// empty success is a claim that the whole corpus was searched and holds nothing,
// and the core abstains on it.

func classify(t *testing.T, resp recall.SearchResponse, err error) (recall.SearchOutcome, string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure, got outcome %q with %d candidates",
			resp.Outcome, len(resp.Candidates))
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Fatalf("a failed search reported success: %v", err)
	}
	if len(resp.Candidates) != 0 {
		t.Fatalf("a failed search returned %d candidates", len(resp.Candidates))
	}
	outcome, reason := adapter.Classify(err)
	return outcome, reason
}

// The cold-start hazard, which is the reason this adapter never treats
// unparseable stdout as an empty result: qmd's first run after an install or a
// model eviction writes a spinner and download progress to STDOUT, ahead of the
// JSON `--json` promises.
func TestColdStartProgressIsBrokenContractNotEmpty(t *testing.T) {
	root := corpus(t)
	progress := "Expanding query... (0ms)\n├─ who can clean my teeth\nSearching 2 queries...\n" +
		"Downloading embeddinggemma-300M-Q8_0.gguf  37%\n"
	runner := healthyRunner(root, progress)
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "who can clean my teeth")
	outcome, _ := classify(t, resp, err)
	if outcome != recall.SearchFailed {
		t.Fatalf("outcome = %q, want failed", outcome)
	}
	if !strings.Contains(err.Error(), "invalid_response") {
		t.Fatalf("error does not name the broken contract: %v", err)
	}
	// The quoted detail is scrubbed: qmd colorizes and animates this output, and
	// it lands in a diagnostic a person reads in a terminal.
	if strings.ContainsAny(err.Error(), "\x1b\r\n") {
		t.Fatalf("error carries control characters: %q", err.Error())
	}
}

// Empty stdout is not an empty result either. `[]` is how qmd spells one, and
// silence means something went wrong before it could say so.
func TestEmptyStdoutIsNotAnEmptyCorpus(t *testing.T) {
	root := corpus(t)
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, ""))
	resp, err := searchOnce(t, a, "dental")
	if _, _ = classify(t, resp, err); !strings.Contains(err.Error(), "invalid_response") {
		t.Fatalf("error = %v", err)
	}
}

// The one empty list that may carry success.
func TestEmptyArrayIsAnHonestSuccess(t *testing.T) {
	root := corpus(t)
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, "[]"))
	resp, err := searchOnce(t, a, "quantum chromodynamics")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 0 {
		t.Fatalf("outcome = %q with %d candidates", resp.Outcome, len(resp.Candidates))
	}
}

func TestNonZeroExitIsUnavailable(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.byKey["query"] = qmd.Result{
		ExitCode: 1,
		Stderr:   []byte("\x1b[31mSqliteError\x1b[0m: unable to open database file\n"),
	}
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "dental")
	outcome, reason := classify(t, resp, err)
	if outcome != recall.SearchUnavailable || reason != "unreachable" {
		t.Fatalf("outcome = %q reason = %q, want unavailable/unreachable", outcome, reason)
	}
	if strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("ANSI escapes reached a diagnostic: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "SqliteError") {
		t.Fatalf("the reason was lost: %v", err)
	}
}

// A missing qmd is the shape an unsatisfied dependency takes. It must degrade
// coverage with this source named, never quietly reduce the answer to whatever
// the lexical adapter found.
func TestMissingBinaryIsUnreachable(t *testing.T) {
	root := corpus(t)
	settings := baseSettings()
	settings["binary"] = "qmd-does-not-exist-anywhere"
	a := qmd.New(qmd.Options{})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "qmd", Location: root, Settings: settings,
	}); err != nil {
		t.Fatalf("the handshake must succeed so health can report the missing binary: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	resp, err := searchOnce(t, a, "dental")
	outcome, reason := classify(t, resp, err)
	if outcome != recall.SearchUnavailable || reason != "unreachable" {
		t.Fatalf("outcome = %q reason = %q", outcome, reason)
	}
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthUnavailable || health.Coverage != recall.IndexUnknown {
		t.Fatalf("health = %q/%q, want unavailable/unknown", health.Status, health.Coverage)
	}
}

// A collection re-pointed at another tree is a store-identity lie: every
// locator, every relative path, and every expansion would answer for a corpus
// this source was never configured for.
func TestCollectionMismatchRefusesEverything(t *testing.T) {
	root := corpus(t)
	elsewhere := t.TempDir()
	runner := healthyRunner(root, "[]")
	runner.byKey["collection"] = qmd.Result{Stdout: []byte(collectionText(elsewhere, "fixture"))}
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "dental")
	outcome, reason := classify(t, resp, err)
	if outcome != recall.SearchUnavailable || reason != "unreachable" {
		t.Fatalf("search outcome = %q reason = %q", outcome, reason)
	}
	if !strings.Contains(err.Error(), "indexes") {
		t.Fatalf("the reason does not name the mismatch: %v", err)
	}
	// No absolute path in the diagnostic: base names only.
	if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), elsewhere) {
		t.Fatalf("a diagnostic carries an absolute path: %v", err)
	}

	if _, err := a.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "qmd", Local: "notes/tooth-care.md#L1-L3"},
		Detail:   recall.DetailExcerpt,
		Deadline: time.Now().Add(time.Minute),
	}); err == nil {
		t.Fatal("expansion served evidence from an unverified corpus")
	}

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Fatalf("health status = %q, want unavailable", health.Status)
	}
}

// A collection qmd does not hold cannot be searched. qmd exits 0 and says so in
// prose, so this is the one place that becomes a source failure.
func TestUnknownCollectionIsUnavailable(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.byKey["collection"] = qmd.Result{Stdout: []byte("Collection not found: fixture\n")}
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "dental")
	if outcome, _ := classify(t, resp, err); outcome != recall.SearchUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
}

// An index holding no vectors cannot answer an embedding-backed query. That is
// not a partial view of the corpus: the search would see nothing, and an empty
// list would claim the corpus holds nothing.
func TestEmbeddingModeWithoutVectorsIsUnavailable(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.byKey["status"] = qmd.Result{Stdout: []byte(statusText(root, "fixture", 2, 0, 2))}
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "dental")
	if outcome, _ := classify(t, resp, err); outcome != recall.SearchUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}

	// The same index answers a bm25 search completely: that mode reads the
	// full-text half and needs no embeddings.
	settings := baseSettings()
	settings["mode"] = "bm25"
	lexical := healthyRunner(root, "[]")
	lexical.byKey["status"] = qmd.Result{Stdout: []byte(statusText(root, "fixture", 2, 0, 2))}
	b := newAdapter(t, root, settings, lexical)
	got, err := searchOnce(t, b, "dental")
	if err != nil {
		t.Fatalf("bm25 must not need vectors: %v", err)
	}
	if got.Outcome != recall.SearchSuccess {
		t.Fatalf("bm25 outcome = %q: %v", got.Outcome, got.Diagnostics)
	}
}

// An index that does not represent every document answers partial, and health
// agrees because both read the same status snapshot.
func TestPartialIndexIsPartialAndDegraded(t *testing.T) {
	root := corpus(t)
	results := "[" + hit("43f92c", "qmd://fixture/notes/tooth-care.md",
		"Tooth care appointment", 5, 5, 2, 1, semanticExplain) + "]"
	runner := healthyRunner(root, results)
	runner.byKey["status"] = qmd.Result{Stdout: []byte(statusText(root, "fixture", 5, 4, 5))}
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %q, want partial", resp.Outcome)
	}
	if _, ok := resp.Diagnostics["coverage_reason"]; !ok {
		t.Error("a partial answer must name what it did not see")
	}
	for _, c := range resp.Candidates {
		if c.ConfirmedAt != nil {
			t.Error("a partial snapshot confirms nothing")
		}
		if c.ObservedAt == nil {
			t.Error("the records were still observed")
		}
	}
	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthDegraded || health.Coverage != recall.IndexPartial {
		t.Fatalf("health = %q/%q, want degraded/partial", health.Status, health.Coverage)
	}
}

// Counts qmd did not report leave coverage unknown. Guessing complete would
// make an unreadable status report look like a fully indexed corpus.
func TestUnreadableStatusLeavesCoverageUnknown(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.byKey["status"] = qmd.Result{Stdout: []byte(
		"QMD Status\n\nCollections\n  fixture (qmd://fixture/)\n    Pattern:  **/*.md\n")}
	a := newAdapter(t, root, baseSettings(), runner)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Coverage != recall.IndexUnknown || health.Status != recall.HealthDegraded {
		t.Fatalf("health = %q/%q, want degraded/unknown", health.Status, health.Coverage)
	}
	resp, err := searchOnce(t, a, "dental")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %q, want partial when coverage is unknown", resp.Outcome)
	}
}

// A result from another collection is dropped and counted, and the count makes
// the answer partial: something matched that this source cannot account for.
func TestForeignCollectionResultDegrades(t *testing.T) {
	root := corpus(t)
	results := "[" +
		hit("43f92c", "qmd://fixture/notes/tooth-care.md", "Tooth care", 5, 5, 2, 1, semanticExplain) + "," +
		hit("aaaaaa", "qmd://elsewhere/secret.md", "Elsewhere", 1, 1, 2, 0.5, semanticExplain) +
		"]"
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, results))

	resp, err := searchOnce(t, a, "dental hygienist")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want the foreign result dropped", len(resp.Candidates))
	}
	if resp.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %q, want partial", resp.Outcome)
	}
	if resp.Diagnostics["foreign_collection_results"] != 1 {
		t.Errorf("diagnostics = %v", resp.Diagnostics)
	}
	if resp.Candidates[0].LocalRank != 1 {
		t.Errorf("ranks must stay contiguous after a drop: %d", resp.Candidates[0].LocalRank)
	}
}

// A deadline that has already passed spends nothing. For a reranked query the
// budget it would have spent is seconds.
func TestElapsedDeadlineDoesNotSpawn(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	a := newAdapter(t, root, baseSettings(), runner)

	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query:    "dental",
		Limit:    10,
		Deadline: time.Now().Add(-time.Minute),
	})
	outcome, reason := classify(t, resp, err)
	if outcome != recall.SearchTimeout || !strings.HasPrefix(reason, "deadline_exceeded") {
		t.Fatalf("outcome = %q reason = %q", outcome, reason)
	}
	if len(runner.invocations()) != 0 {
		t.Fatalf("an expired request spawned %v", runner.invocations())
	}
}

// A search cancelled while qmd is still running reports the timeout and returns
// no candidates. A late result is worse than an error: the core has already told
// its caller this source did not answer.
func TestCancellationInFlight(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.delay = 10 * time.Second
	a := newAdapter(t, root, baseSettings(), runner)

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
		t.Fatalf("cancellation waited out the recorded delay: %s", elapsed)
	}
	outcome, _ := classify(t, resp, err)
	if outcome != recall.SearchTimeout {
		t.Fatalf("outcome = %q, want timeout", outcome)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a cancellation", err)
	}
}

// A refresh that fails reports through health, never as an error: a frame
// carries a result or an error and never both, so erroring would discard the
// health of the index that is still there and still answering.
func TestFailedRefreshReturnsHealth(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	runner.byKey["embed"] = qmd.Result{ExitCode: 1, Stderr: []byte("model file is missing\n")}
	a := newAdapter(t, root, baseSettings(), runner)

	health, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil {
		t.Fatalf("a failed build must not error: %v", err)
	}
	if health.Status == recall.HealthHealthy {
		t.Fatal("a refresh that did not move the index forward reported healthy")
	}
	detail, _ := health.Diagnostics["last_refresh_error"].(string)
	if !strings.Contains(detail, "embed") {
		t.Fatalf("diagnostics do not name the failed step: %v", health.Diagnostics)
	}
}

// An as_of request is refused rather than answered from current state. The core
// already excludes a source declaring none; refusing again is what keeps that
// true if it ever stops.
func TestAsOfIsRefused(t *testing.T) {
	root := corpus(t)
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, "[]"))
	boundary := time.Now().Add(-24 * time.Hour)
	resp, err := a.Search(context.Background(), recall.SearchRequest{
		Query: "dental", Limit: 10, AsOf: &boundary,
		Deadline: time.Now().Add(time.Minute),
	})
	if _, reason := classify(t, resp, err); reason != "as_of_unsupported" {
		t.Fatalf("reason = %q", reason)
	}
}

// Filters this source cannot evaluate are declined before retrieval. A broader
// result set is not evidence for the narrower question, even labelled partial.
func TestUnsupportedFiltersSkipBeforeRetrieval(t *testing.T) {
	root := corpus(t)
	since := time.Now().Add(-24 * time.Hour)
	for name, req := range map[string]recall.SearchRequest{
		"since":       {Query: "dental", Filters: recall.Filters{Since: &since}},
		"project":     {Query: "dental", Filters: recall.Filters{Project: "recall"}},
		"entities":    {Query: "dental", Filters: recall.Filters{Entities: []string{"dana"}}},
		"record_type": {Query: "dental", Filters: recall.Filters{RecordTypes: []recall.RecordType{recall.RecordTask}}},
		"empty_query": {Query: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			runner := healthyRunner(root, "[]")
			a := newAdapter(t, root, baseSettings(), runner)
			req.Limit = 10
			req.Deadline = time.Now().Add(time.Minute)
			resp, err := a.Search(context.Background(), req)
			if err != nil {
				t.Fatalf("a skip is not a failure: %v", err)
			}
			if resp.Outcome != recall.SearchSkipped {
				t.Fatalf("outcome = %q, want skipped", resp.Outcome)
			}
			if resp.Reason == "" {
				t.Fatal("an unstated skip reason degrades and says nothing")
			}
			if len(resp.Candidates) != 0 {
				t.Fatalf("a skipped response carried %d candidates", len(resp.Candidates))
			}
			if len(runner.invocations()) != 0 {
				t.Fatalf("a skip decided after retrieval: %v", runner.invocations())
			}
		})
	}
}

// A closed adapter is closed. It must not answer from whatever it last held.
func TestClosedAdapterAnswersNothing(t *testing.T) {
	root := corpus(t)
	a := newAdapter(t, root, baseSettings(), healthyRunner(root, "[]"))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := searchOnce(t, a, "dental")
	if _, _ = classify(t, resp, err); !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("error = %v, want closed", err)
	}
	if _, err := a.Health(context.Background()); !errors.Is(err, adapter.ErrClosed) {
		t.Fatalf("health error = %v, want closed", err)
	}
}
