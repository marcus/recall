package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// fake is a scriptable adapter. Every way a real source misbehaves is a field
// here rather than a separate type, so a test reads as the situation it models.
type fake struct {
	// needsHandshake models a real built-in adapter: it is constructed
	// unconfigured and cannot say anything about a source until Initialize has
	// told it where to read.
	needsHandshake bool
	initialized    bool

	manifest         recall.Manifest
	health           recall.Health
	healthErr        error
	candidates       []recall.Candidate
	outcome          recall.SearchOutcome
	searchErr        error
	delay            time.Duration
	delayAfterCancel time.Duration
	evidence         recall.ExpandResponse
	expandErr        error
	initializeErr    error
	refreshHealth    recall.Health
	refreshErr       error
	refreshWait      bool
	refreshCalls     int
	refreshFull      bool
	refreshCancelled bool
	searchCalls      int
	healthCalls      int

	// prepared opts this fake into adapter.PreparedSearcher without making
	// every app test stop exercising the ordinary adapter contract.
	prepared            bool
	prepareCalls        int
	preparedSearchCalls int
	sawOwnPreparation   bool

	// project is the project this fake serves, and sawProject is what the last
	// search was actually asked for. Together they are how an end-to-end test
	// reaches Filters.Project, which no host surface populated until scope
	// gained it.
	project     string
	sawProject  string
	sawEntities []string
	reason      string
}

func (f *fake) Initialize(context.Context, adapter.Config) (recall.Manifest, error) {
	f.initialized = true
	return f.manifest, f.initializeErr
}

func (f *fake) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	f.searchCalls++
	f.sawProject = req.Filters.Project
	f.sawEntities = append([]string(nil), req.Filters.Entities...)
	if f.project != "" && req.Filters.Project != "" && !strings.EqualFold(f.project, req.Filters.Project) {
		// What a real routed source does: it is not the one that was named, so
		// it did not look. Success here would assert a boundary it never
		// crossed.
		return recall.SearchResponse{
			Outcome: recall.SearchSkipped, Reason: recall.SkipNotApplicable,
		}, nil
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			if f.delayAfterCancel > 0 {
				time.Sleep(f.delayAfterCancel)
			}
			return recall.SearchResponse{Outcome: recall.SearchTimeout}, ctx.Err()
		}
	}
	if f.searchErr != nil {
		return recall.SearchResponse{Outcome: recall.SearchUnavailable}, f.searchErr
	}
	out := f.outcome
	if out == "" {
		out = recall.SearchSuccess
	}
	return recall.SearchResponse{Candidates: f.candidates, Outcome: out, Reason: f.reason}, nil
}

func (f *fake) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	return f.evidence, f.expandErr
}

func (f *fake) Health(context.Context) (recall.Health, error) {
	f.healthCalls++
	if f.needsHandshake && !f.initialized {
		return recall.Health{Status: recall.HealthUnavailable}, protocol.ErrSourceUnavailable
	}
	if f.healthErr != nil {
		return f.health, f.healthErr
	}
	h := f.health
	if h.Status == "" {
		h.Status = recall.HealthHealthy
		h.Coverage = recall.IndexComplete
	}
	return h, nil
}

func (f *fake) Refresh(ctx context.Context, p protocol.RefreshParams) (recall.Health, error) {
	f.refreshCalls++
	f.refreshFull = p.Full
	if f.refreshWait {
		<-ctx.Done()
		f.refreshCancelled = true
		return recall.Health{}, ctx.Err()
	}
	if f.refreshErr != nil {
		return f.refreshHealth, f.refreshErr
	}
	if f.refreshHealth.Status != "" {
		return f.refreshHealth, nil
	}
	return f.Health(ctx)
}

func (f *fake) Close() error { return nil }

type preparedFake struct {
	*fake
}

func (f *preparedFake) PrepareSearch(
	ctx context.Context,
	_ recall.SearchRequest,
) (recall.Health, adapter.SearchPreparation, error) {
	f.prepareCalls++
	health, err := f.Health(ctx)
	return health, adapter.SearchPreparation{State: f.fake}, err
}

func (f *preparedFake) SearchPrepared(
	ctx context.Context,
	req recall.SearchRequest,
	preparation adapter.SearchPreparation,
) (recall.SearchResponse, error) {
	f.preparedSearchCalls++
	f.sawOwnPreparation = preparation.State == f.fake
	return f.Search(ctx, req)
}

func manifest(types ...recall.RecordType) recall.Manifest {
	return recall.Manifest{
		ProtocolVersion: 1,
		AdapterID:       "fake/1",
		RecordTypes:     types,
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessIndexed},
		AsOfSupport:     recall.AsOfFilter,
		RelevanceBasis:  recall.RelevanceLexicalSpan,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
	}
}

func cand(sourceID, local string, rank int, opts ...func(*recall.Candidate)) recall.Candidate {
	c := recall.Candidate{
		SourceRecordID: local,
		Locator:        recall.Locator{SourceID: sourceID, Local: local},
		RecordType:     recall.RecordDocument,
		Title:          "title " + local,
		Excerpt:        "excerpt for " + local,
		LocalRank:      rank,
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// relevance is the source's estimate of how much a record is about the query,
// on the one definition every source computes the same way. It is the only
// relevance signal fusion may compare across sources, and therefore the only
// one the profile's floor can be written in.
func relevance(v float64) func(*recall.Candidate) {
	return func(c *recall.Candidate) { c.Relevance = &v }
}

// harness builds a real config, a real registry, and a real ranker over fake
// adapters, so eligibility, identity stamping, and the ceiling are all the
// production code paths.
type harness struct {
	app    *app.App
	fakes  map[string]*fake
	config *config.Config
}

const configTOML = `
[defaults]
profile = "work"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
location = "/tmp/docs"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[[sources]]
source_uid = "01UIDTASKS"
source_id = "tasks"
adapter = "faketasks"
location = "/tmp/tasks"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[[sources]]
source_uid = "01UIDVAULT"
source_id = "vault"
adapter = "fakevault"
location = "/tmp/vault"
freshness_mode = "indexed"
sensitivity = "restricted"
base_prior = 1.0

[profiles.work]
sources = ["docs", "tasks", "vault"]
max_sensitivity = "internal"
`

func newHarness(t *testing.T, tune func(map[string]*fake)) *harness {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "recall")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(configTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	builtinNames := []string{"fakedocs", "faketasks", "fakevault"}
	var builtins []config.Builtin
	for _, n := range builtinNames {
		builtins = append(builtins, config.Builtin{
			Name:           n,
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		})
	}

	cfg, err := config.Load(config.Options{
		Paths: config.Paths{
			ConfigHome: home,
			StateHome:  filepath.Join(home, "state"),
			CacheHome:  filepath.Join(home, "cache"),
		},
		Builtins: builtins,
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	fakes := map[string]*fake{}
	for _, n := range builtinNames {
		fakes[n] = &fake{manifest: manifest(recall.RecordDocument)}
	}
	if tune != nil {
		tune(fakes)
	}

	factories := map[string]source.Factory{}
	for name, f := range fakes {
		factories[name] = func() adapter.Adapter {
			if f.prepared {
				return &preparedFake{fake: f}
			}
			return f
		}
	}

	reg := source.NewRegistry(cfg, source.Options{
		Builtins: factories,
		StateDir: t.TempDir(),
	})
	t.Cleanup(func() { _ = reg.Close() })

	// Through the production mapping rather than a hand-written ranking.Config:
	// app.RankingConfig is the one translation from configuration to fusion, and
	// a harness that reimplemented it would test priors and volume rules nobody
	// runs. The configured priors are all 1.0, so nothing about ordering changes.
	ranker, err := ranking.New(app.RankingConfig(cfg, 0))
	if err != nil {
		t.Fatal(err)
	}

	return &harness{
		app:    app.New(app.Options{Config: cfg, Registry: reg, Ranker: ranker}),
		fakes:  fakes,
		config: cfg,
	}
}

func query(q string) recall.QueryRequest {
	return recall.QueryRequest{
		Query:  q,
		Mode:   recall.ModeExplicit,
		Budget: recall.Budget{LatencyMS: 2000},
		Limit:  10,
	}
}

func reportFor(t *testing.T, resp recall.QueryResponse, id string) recall.SourceReport {
	t.Helper()
	for _, r := range resp.SourceOutcomes {
		if r.SourceID == id {
			return r
		}
	}
	t.Fatalf("no outcome reported for %q; a source that did not answer must still be visible", id)
	return recall.SourceReport{}
}

// Query classification belongs in the application core, before fusion. This
// test is deliberately above ranking: CLI, API, MCP, and eval all call this
// path and must not grow separate opinions about when exact means lookup.
func TestQueryClassControlsExactPromotionThroughApplicationCore(t *testing.T) {
	low, high := 0.1, 0.9
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{
			cand("docs", "answer.md#1", 1, func(c *recall.Candidate) {
				c.Relevance = &high
			}),
		}
		f["faketasks"].candidates = []recall.Candidate{
			cand("tasks", "project-health", 20, func(c *recall.Candidate) {
				c.Relevance = &low
				c.MatchSignals = []recall.MatchSignal{recall.MatchExactIdentifier}
			}),
		}
	})

	tests := []struct {
		name      string
		query     string
		wantFirst string
		promoted  bool
	}{
		{"project subject clara", "how does clara decide what to remember", "docs:answer.md#1", false},
		{"project subject braid", "what is braid's daily podcast pipeline", "docs:answer.md#1", false},
		{"unlisted prose verb", "summarize braid's daily podcast pipeline", "docs:answer.md#1", false},
		{"weak version token", "how does clara v2 remember things?", "docs:answer.md#1", false},
		{"one word clara", "clara", "tasks:project-health", true},
		{"one word sidecar", "sidecar", "tasks:project-health", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.app.Query(context.Background(), query(tc.query))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Results) != 2 {
				t.Fatalf("results = %d, want 2", len(resp.Results))
			}
			if got := resp.Results[0].Primary.Locator.String(); got != tc.wantFirst {
				t.Fatalf("first = %s, want %s", got, tc.wantFirst)
			}
			var health recall.Result
			for _, result := range resp.Results {
				if result.Primary.Locator.String() == "tasks:project-health" {
					health = result
				}
			}
			if !health.Primary.HasSignal(recall.MatchExactIdentifier) {
				t.Fatal("project-health lost its exact_identifier signal")
			}
			if health.Explanation.ExactPromoted != tc.promoted {
				t.Errorf("exact_promoted = %v, want %v", health.Explanation.ExactPromoted, tc.promoted)
			}
		})
	}
}

func TestApplicationCorrelatesStableIdentifierToExactCandidate(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		target string
	}{
		{"td id beside project name", "how does clara address td-6c98c1?", "td-6c98c1"},
		{"tasks compact id", "What is the state of aaaa0001?", "aaaa0001"},
		{"ongoing underscore id", "What is the state of project_recall?", "project_recall"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			low, high := 0.1, 0.9
			h := newHarness(t, func(f map[string]*fake) {
				f["fakedocs"].candidates = []recall.Candidate{
					cand("docs", "answer.md#1", 1, func(c *recall.Candidate) {
						c.Relevance = &high
					}),
				}
				f["faketasks"].candidates = []recall.Candidate{
					cand("tasks", "clara", 1, func(c *recall.Candidate) {
						c.Relevance = &low
						c.MatchSignals = []recall.MatchSignal{recall.MatchExactIdentifier}
					}),
					cand("tasks", tc.target, 2, func(c *recall.Candidate) {
						c.Relevance = &low
						c.MatchSignals = []recall.MatchSignal{recall.MatchExactIdentifier}
					}),
				}
			})

			resp, err := h.app.Query(context.Background(), query(tc.query))
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.Results[0].Primary.Locator.Local; got != tc.target {
				t.Fatalf("first = %s, want named identifier %s", got, tc.target)
			}
			if !resp.Results[0].Explanation.ExactPromoted {
				t.Fatal("named exact candidate did not partition")
			}
			for _, result := range resp.Results {
				if result.Primary.Locator.Local == "clara" && result.Explanation.ExactPromoted {
					t.Fatal("unrelated exact Clara candidate partitioned")
				}
			}
		})
	}
}

// The application core is the shared surface for CLI, API, MCP, and eval. A
// ranking-only test is not enough for the document representative rule: source
// identity stamping and response annotation happen here, after adapters answer.
func TestApplicationShowsMatchedChunkWithoutReattributingDocumentScore(t *testing.T) {
	headingRelevance, contentRelevance := 0.95, 0.40
	headingObserved := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	contentObserved := headingObserved.Add(time.Minute)
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{
			cand("docs", "research.md#L1-L1", 1, func(c *recall.Candidate) {
				c.SourceRecordID = "research.md"
				c.ExcerptKind = recall.ExcerptPreview
				c.Relevance = &headingRelevance
				c.ObservedAt = &headingObserved
			}),
			cand("docs", "research.md#L20-L27", 2, func(c *recall.Candidate) {
				c.SourceRecordID = "research.md"
				c.ExcerptKind = recall.ExcerptMatched
				c.Relevance = &contentRelevance
				c.ObservedAt = &contentObserved
			}),
		}
	})

	resp, err := h.app.Query(context.Background(), query("dentist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want one clustered document", len(resp.Results))
	}
	got := resp.Results[0]
	if got.Primary.Locator.Local != "research.md#L20-L27" {
		t.Fatalf("primary = %s, want the matched content chunk", got.Primary.Locator.Local)
	}
	if got.Explanation.LineageRoot != "01UIDDOCS:research.md#L1-L1" ||
		got.Explanation.LocalRank != 1 ||
		got.Explanation.Relevance == nil ||
		*got.Explanation.Relevance != headingRelevance {
		t.Errorf("score basis = %+v, want the preview heading's scoring evidence",
			got.Explanation)
	}
	if got.Explanation.Score != got.Score {
		t.Errorf("explanation claims %v but result scored %v", got.Explanation.Score, got.Score)
	}
	if got.Explanation.Freshness.ObservedAt == nil ||
		!got.Explanation.Freshness.ObservedAt.Equal(headingObserved) {
		t.Errorf("score-basis observation = %v, want heading observation %v",
			got.Explanation.Freshness.ObservedAt, headingObserved)
	}
	if len(got.Members) != 2 {
		t.Errorf("members = %d, want both chunks retrievable", len(got.Members))
	}
}

func checkpointManifest() recall.Manifest {
	m := manifest(recall.RecordDocument)
	m.Capabilities = append(m.Capabilities, recall.CapCheckpoint)
	return m
}

func TestRefreshAllReportsMixedSourceOutcomesAndKeepsProfileOrder(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].manifest = checkpointManifest()
		f["fakedocs"].refreshHealth = recall.Health{
			Status: recall.HealthHealthy, Coverage: recall.IndexComplete,
			IndexGeneration: "generation-2",
		}
		f["faketasks"].manifest = checkpointManifest()
		f["faketasks"].refreshErr = errors.New("adapter unavailable")
	})

	resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{Profile: "work", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.RefreshDegraded {
		t.Fatalf("outcome = %s, want degraded", resp.Outcome)
	}
	if len(resp.Sources) != 3 {
		t.Fatalf("sources = %d, want every profile member reported", len(resp.Sources))
	}
	if got := []string{resp.Sources[0].SourceID, resp.Sources[1].SourceID, resp.Sources[2].SourceID}; strings.Join(got, ",") != "docs,tasks,vault" {
		t.Fatalf("source order = %v, want profile order", got)
	}
	if resp.Sources[0].Status != recall.RefreshSourceRefreshed ||
		resp.Sources[0].Health == nil || resp.Sources[0].Health.IndexGeneration != "generation-2" {
		t.Errorf("docs outcome = %+v", resp.Sources[0])
	}
	if !h.fakes["fakedocs"].refreshFull {
		t.Error("full refresh flag did not reach adapter")
	}
	if resp.Sources[1].Status != recall.RefreshSourceFailed ||
		resp.Sources[1].Reason != recall.RefreshOperationFailed {
		t.Errorf("tasks outcome = %+v", resp.Sources[1])
	}
	if resp.Sources[2].Status != recall.RefreshSourceSkipped ||
		resp.Sources[2].Reason != recall.RefreshDenied {
		t.Errorf("vault outcome = %+v", resp.Sources[2])
	}
}

func TestRefreshForwardProgressDoesNotRelabelPartialHealth(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].manifest = checkpointManifest()
		f["fakedocs"].refreshHealth = recall.Health{
			Status:             recall.HealthDegraded,
			Coverage:           recall.IndexPartial,
			CheckpointProgress: recall.CheckpointAdvanced,
			Diagnostics:        map[string]any{"detail": "one new document arrived during refresh"},
		}
	})
	resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{SourceID: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	got := resp.Sources[0]
	if got.Status != recall.RefreshSourceRefreshed || resp.Outcome != recall.RefreshSucceeded {
		t.Fatalf("refresh response = %+v", resp)
	}
	if got.Health == nil || got.Health.Status != recall.HealthDegraded ||
		got.Health.Coverage != recall.IndexPartial {
		t.Fatalf("attached health = %+v, want degraded partial", got.Health)
	}
	if got.DiagnosticDetail != "one new document arrived during refresh" {
		t.Fatalf("diagnostic detail = %q", got.DiagnosticDetail)
	}
}

func TestRefreshDoesNotAcceptNonAdvancingPartialHealth(t *testing.T) {
	for _, progress := range []recall.CheckpointProgress{
		"", recall.CheckpointUnchanged, recall.CheckpointRegressed,
	} {
		t.Run(string(progress), func(t *testing.T) {
			h := newHarness(t, func(f map[string]*fake) {
				f["fakedocs"].manifest = checkpointManifest()
				f["fakedocs"].refreshHealth = recall.Health{
					Status: recall.HealthDegraded, Coverage: recall.IndexPartial,
					CheckpointProgress: progress,
				}
			})
			resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{SourceID: "docs"})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Sources[0].Status != recall.RefreshSourceDegraded ||
				resp.Outcome != recall.RefreshDegraded {
				t.Fatalf("progress %q response = %+v", progress, resp)
			}
		})
	}
}

func TestRefreshNamedLiveSourceProbesHealthWithoutCallingRefresh(t *testing.T) {
	detail := "tasks store is current"
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].health = recall.Health{
			Status: recall.HealthHealthy, Coverage: recall.IndexComplete,
			Diagnostics: map[string]any{"detail": detail},
		}
	})
	resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{SourceID: "tasks", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.RefreshSucceeded || len(resp.Sources) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	got := resp.Sources[0]
	if got.Status != recall.RefreshSourceRefreshed || got.Reason != "" {
		t.Fatalf("source = %+v", got)
	}
	if got.Health == nil || got.Health.Status != recall.HealthHealthy ||
		got.Health.Coverage != recall.IndexComplete || got.DiagnosticDetail != detail {
		t.Fatalf("attached health = %+v detail=%q", got.Health, got.DiagnosticDetail)
	}
	tasks := h.fakes["faketasks"]
	if tasks.healthCalls != 1 {
		t.Fatalf("Health calls = %d, want 1", tasks.healthCalls)
	}
	if tasks.refreshCalls != 0 {
		t.Fatalf("Refresh calls = %d, want 0 on a live named source", tasks.refreshCalls)
	}
}

func TestRefreshNamedLiveSourceReportsDegradedAndUnavailableHealth(t *testing.T) {
	detail := "tasks CLI cannot list the store"
	for _, tc := range []struct {
		name      string
		health    recall.Health
		healthErr error
		status    recall.RefreshSourceStatus
		reason    recall.RefreshReason
		outcome   recall.RefreshOutcome
	}{
		{
			name: "degraded",
			health: recall.Health{
				Status: recall.HealthDegraded, Coverage: recall.IndexPartial,
				CheckpointProgress: recall.CheckpointAdvanced,
				Diagnostics:        map[string]any{"detail": detail},
			},
			status:  recall.RefreshSourceDegraded,
			reason:  recall.RefreshUnhealthy,
			outcome: recall.RefreshDegraded,
		},
		{
			name: "unavailable",
			health: recall.Health{
				Status: recall.HealthUnavailable, Coverage: recall.IndexUnknown,
				Diagnostics: map[string]any{"last_refresh_error": detail},
			},
			status:  recall.RefreshSourceFailed,
			reason:  recall.RefreshUnhealthy,
			outcome: recall.RefreshFailed,
		},
		{
			name: "probe error",
			health: recall.Health{
				Status: recall.HealthUnavailable, Coverage: recall.IndexUnknown,
				Diagnostics: map[string]any{"detail": detail},
			},
			healthErr: context.DeadlineExceeded,
			status:    recall.RefreshSourceFailed,
			reason:    recall.RefreshTimedOut,
			outcome:   recall.RefreshFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(f map[string]*fake) {
				f["faketasks"].health = tc.health
				f["faketasks"].healthErr = tc.healthErr
			})
			resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{SourceID: "tasks"})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Outcome != tc.outcome || len(resp.Sources) != 1 {
				t.Fatalf("response = %+v", resp)
			}
			got := resp.Sources[0]
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("source = %+v, want status %s reason %s", got, tc.status, tc.reason)
			}
			if got.DiagnosticDetail != detail {
				t.Fatalf("diagnostic = %q, want %q", got.DiagnosticDetail, detail)
			}
			tasks := h.fakes["faketasks"]
			if tasks.refreshCalls != 0 {
				t.Fatalf("Refresh calls = %d, want 0", tasks.refreshCalls)
			}
			if tasks.healthCalls != 1 {
				t.Fatalf("Health calls = %d, want 1", tasks.healthCalls)
			}
		})
	}
}

func TestRefreshNamedMissingSourceIsNotConfigured(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{SourceID: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.RefreshFailed || len(resp.Sources) != 1 ||
		resp.Sources[0].Reason != recall.RefreshSourceNotConfigured {
		t.Fatalf("response = %+v", resp)
	}
}

func TestRefreshAllSkipsLiveSourcesWithoutDegradingUsableRefresh(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].manifest = checkpointManifest()
		f["fakedocs"].refreshHealth = recall.Health{
			Status: recall.HealthHealthy, Coverage: recall.IndexComplete,
		}
	})
	resp, err := h.app.Refresh(context.Background(), recall.RefreshRequest{Profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.RefreshSucceeded {
		t.Fatalf("outcome = %s, want refreshed when the live source was skipped", resp.Outcome)
	}
	if len(resp.Sources) != 3 {
		t.Fatalf("sources = %d, want every profile member reported", len(resp.Sources))
	}
	if resp.Sources[1].SourceID != "tasks" ||
		resp.Sources[1].Status != recall.RefreshSourceSkipped ||
		resp.Sources[1].Reason != recall.RefreshCheckpointUnsupported {
		t.Fatalf("tasks = %+v, want skipped checkpoint_unsupported", resp.Sources[1])
	}
	tasks := h.fakes["faketasks"]
	if tasks.refreshCalls != 0 {
		t.Fatalf("Refresh calls = %d, want 0 on an all-source skip", tasks.refreshCalls)
	}
	if tasks.healthCalls != 0 {
		t.Fatalf("Health calls = %d, want 0 on an all-source skip", tasks.healthCalls)
	}
}

func TestRefreshCancellationReachesAdapterAndIsTyped(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].manifest = checkpointManifest()
		f["fakedocs"].refreshWait = true
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := h.app.Refresh(ctx, recall.RefreshRequest{SourceID: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.fakes["fakedocs"].refreshCancelled {
		t.Fatal("cancelled context did not reach adapter refresh")
	}
	if resp.Outcome != recall.RefreshFailed || resp.Sources[0].Reason != recall.RefreshCancelled {
		t.Fatalf("response = %+v, want typed cancelled failure", resp)
	}
}

func TestRefreshSourceTimeoutCoversRealDocumentInitialization(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "recall")
	corpusDir := filepath.Join(home, "corpus")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "proof.md"), []byte("# Proof\n\nCold build.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const raw = `
[defaults]
profile = "work"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "documents"
location = "CORPUS"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs"]
max_sensitivity = "internal"
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"),
		[]byte(strings.ReplaceAll(raw, "CORPUS", corpusDir)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{
		Paths: config.Paths{
			ConfigHome: home,
			StateHome:  filepath.Join(home, "state"),
			CacheHome:  filepath.Join(home, "cache"),
		},
		Builtins: []config.Builtin{{
			Name: "documents", FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	inst, ok := cfg.Source("docs")
	if !ok {
		t.Fatal("documents source missing")
	}
	// Mutate after validated load so the test can deterministically expire
	// between App entering the source and the adapter's cold handshake.
	inst.Timeout = time.Nanosecond
	reg := source.NewRegistry(cfg, source.Options{
		Builtins: map[string]source.Factory{
			"documents": func() adapter.Adapter { return docs.New() },
		},
		StateDir: filepath.Join(home, "adapter-state"),
	})
	t.Cleanup(func() { _ = reg.Close() })
	core := app.New(app.Options{Config: cfg, Registry: reg})

	resp, err := core.Refresh(t.Context(), recall.RefreshRequest{SourceID: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.RefreshFailed || len(resp.Sources) != 1 ||
		resp.Sources[0].Reason != recall.RefreshTimedOut {
		t.Fatalf("cold documents refresh = %+v, want typed source timeout", resp)
	}
}

// A source classified above the profile ceiling is never asked at all.
// Filtering its results afterwards would still have sent it the query, which is
// itself a disclosure.
func TestSourceAboveTheCeilingIsNeverAsked(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
		f["fakevault"].candidates = []recall.Candidate{cand("vault", "secret", 1)}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if h.fakes["fakevault"].searchCalls != 0 {
		t.Error("a source above the ceiling was queried")
	}
	rep := reportFor(t, resp, "vault")
	if rep.Reason != source.ReasonSensitivity {
		t.Errorf("vault reason = %q, want %q", rep.Reason, source.ReasonSensitivity)
	}
	// The ceiling is the user's own policy, so honoring it is not a
	// degradation. Reporting one here would make every well-configured query
	// look impaired and the signal would stop meaning anything.
	if resp.Coverage != recall.CoverageComplete {
		t.Errorf("coverage = %s; a source excluded by the configured ceiling is not degradation",
			resp.Coverage)
	}
	for _, r := range resp.Results {
		if r.Primary.SourceID == "vault" {
			t.Error("vault evidence reached the response")
		}
	}
}

// A source may be permitted while an individual record is not: an adapter may
// classify a record more restrictively than its source.
func TestCandidateAboveTheCeilingIsDroppedAndCounted(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{
			cand("docs", "public.md", 1),
			cand("docs", "sealed.md", 2, func(c *recall.Candidate) {
				c.Sensitivity = recall.SensitivityRestricted
			}),
		}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Results {
		if strings.Contains(r.Primary.Locator.Local, "sealed") {
			t.Fatal("a record above the ceiling was shown")
		}
	}
	var counted int
	for _, s := range resp.Suppressed {
		if s.Reason == recall.SuppressSensitivity {
			counted += s.Count
		}
	}
	if counted != 1 {
		t.Errorf("sensitivity suppressions = %d, want 1 counted so a host can say something was withheld", counted)
	}
}

// A source that cannot honor a historical boundary is excluded and says so.
// Answering from current state would be a wrong answer shaped like a right one.
func TestAsOfExcludesSourcesThatCannotHonorIt(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		m := manifest(recall.RecordDocument)
		m.AsOfSupport = recall.AsOfNone
		f["faketasks"].manifest = m
		f["faketasks"].candidates = []recall.Candidate{cand("tasks", "td-1", 1)}
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
	})

	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	req := query("what did we decide")
	req.AsOf = &at

	resp, err := h.app.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if h.fakes["faketasks"].searchCalls != 0 {
		t.Error("a source declaring as_of_support none was asked a historical question")
	}
	if rep := reportFor(t, resp, "tasks"); rep.Reason != source.ReasonAsOfUnsupported {
		t.Errorf("tasks reason = %q, want %q", rep.Reason, source.ReasonAsOfUnsupported)
	}
	var excluded *recall.PlanSource
	for i := range resp.Plan.Sources {
		if resp.Plan.Sources[i].SourceID == "tasks" {
			excluded = &resp.Plan.Sources[i]
			break
		}
	}
	if excluded == nil {
		t.Fatal("tasks is missing from the resolved plan")
	}
	if excluded.Eligible || excluded.RelevanceBasis != recall.RelevanceLexicalSpan {
		t.Errorf("excluded tasks plan = %+v, want ineligible with known lexical_span basis", *excluded)
	}
	if resp.Coverage != recall.CoverageDegraded {
		t.Errorf("coverage = %s, want degraded", resp.Coverage)
	}
}

// The invariant with the sharpest failure mode: an unreachable source must
// never look like a source with nothing to say.
func TestUnreachableSourceIsNotAnEmptySuccess(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].searchErr = protocol.ErrSourceUnavailable
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Outcome.Searched() {
		t.Errorf("outcome = %s, want a failure", rep.Outcome)
	}
	if rep.Reason != "unreachable" {
		t.Errorf("reason = %q, want unreachable", rep.Reason)
	}
	if resp.Coverage != recall.CoverageDegraded {
		t.Errorf("coverage = %s, want degraded", resp.Coverage)
	}
	// The healthy source still answered. One source failing is degraded
	// coverage, not a failed request.
	if resp.Outcome != recall.OutcomeAnswered {
		t.Errorf("outcome = %s, want answered", resp.Outcome)
	}
}

// Nothing found is only an answer if something looked.
func TestEveryAskedSourceFailingIsFailedNotAbstained(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].searchErr = protocol.ErrSourceUnavailable
		f["faketasks"].searchErr = protocol.ErrSourceUnavailable
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.OutcomeFailed {
		t.Errorf("outcome = %s, want failed: no source answered, so no-results is a claim nothing supports", resp.Outcome)
	}
}

func TestNoMatchesIsAbstained(t *testing.T) {
	h := newHarness(t, nil) // every source healthy, none returns anything

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.OutcomeAbstained {
		t.Errorf("outcome = %s, want abstained", resp.Outcome)
	}
	if resp.Coverage != recall.CoverageComplete {
		t.Errorf("coverage = %s; abstaining with every source healthy is complete coverage", resp.Coverage)
	}
}

// A source slower than its budget is reported as timed out, and the request
// still returns. The deadline is the caller's, not the slowest source's.
func TestSlowSourceTimesOutWithoutHoldingTheRequest(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].delay = 3 * time.Second
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
	})

	req := query("anything")
	req.Budget.LatencyMS = 200

	start := time.Now()
	resp, err := h.app.Query(context.Background(), req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("query took %v against a 200ms budget", elapsed)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Outcome.Searched() {
		t.Errorf("tasks outcome = %s, want a failure", rep.Outcome)
	}
	if rep.Timeout == nil || rep.Timeout.Budget != recall.TimeoutRequestLatency ||
		rep.Timeout.Limit != 200*time.Millisecond {
		t.Fatalf("timeout detail = %+v, want the 200ms request latency budget", rep.Timeout)
	}
	if got := source.DegradedReports(resp.SourceOutcomes); len(got) == 0 ||
		!strings.Contains(got[0], "request latency budget 200ms") {
		t.Fatalf("degraded reports = %v", got)
	}
	if resp.Coverage != recall.CoverageDegraded {
		t.Errorf("coverage = %s, want degraded", resp.Coverage)
	}
}

func TestTimeoutNamesTheConfiguredSourceBudgetWhenItIsEarlier(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].delay = time.Second
	})
	inst, ok := h.config.Source("tasks")
	if !ok {
		t.Fatal("tasks source missing")
	}
	inst.Timeout = 40 * time.Millisecond

	req := query("anything")
	req.Budget.LatencyMS = 1000
	resp, err := h.app.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Timeout == nil || rep.Timeout.Budget != recall.TimeoutSourceLimit ||
		rep.Timeout.Limit != 40*time.Millisecond {
		t.Fatalf("timeout detail = %+v, want the 40ms source timeout", rep.Timeout)
	}
}

func TestTimeoutDoesNotBlameCoreBudgetsForAnEarlierAdapterDeadline(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].searchErr = context.DeadlineExceeded
	})
	req := query("anything")
	req.Budget.LatencyMS = 1000
	resp, err := h.app.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Timeout == nil || rep.Timeout.Budget != recall.TimeoutAdapterInternal {
		t.Fatalf("timeout detail = %+v, want adapter_internal", rep.Timeout)
	}
}

func TestCallerCancellationIsNotAttributedToATimeoutBudget(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].delay = time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := h.app.Query(ctx, query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Outcome != recall.SearchFailed || rep.Reason != "cancelled" {
		t.Fatalf("cancelled source report = %+v", rep)
	}
	if rep.Timeout != nil {
		t.Fatalf("caller cancellation was attributed to timeout budget %+v", rep.Timeout)
	}
	got := source.DegradedReports(resp.SourceOutcomes)
	named := false
	for _, report := range got {
		named = named || strings.Contains(report, "cancelled")
	}
	if !named {
		t.Fatalf("degraded reports = %v, want cancellation named", got)
	}
}

func TestEarlierCallerDeadlineIsAttributedToTheCaller(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].delay = time.Second
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	req := query("anything")
	req.Budget.LatencyMS = 1000

	resp, err := h.app.Query(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Outcome != recall.SearchTimeout || rep.Reason != "timeout" ||
		rep.Timeout == nil || rep.Timeout.Budget != recall.TimeoutCallerDeadline {
		t.Fatalf("caller-deadline source report = %+v", rep)
	}
	if rep.Timeout.Deadline == nil || !rep.Timeout.Deadline.Equal(parentDeadline) {
		t.Fatalf("reported deadline = %v, want caller deadline %s",
			rep.Timeout.Deadline, parentDeadline)
	}
}

func TestLaterCallerDeadlineCannotStealSourceTimeoutAttribution(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["faketasks"].delay = time.Second
		f["faketasks"].delayAfterCancel = 80 * time.Millisecond
	})
	inst, ok := h.config.Source("tasks")
	if !ok {
		t.Fatal("tasks source missing")
	}
	inst.Timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	req := query("anything")
	req.Budget.LatencyMS = 1000

	resp, err := h.app.Query(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	rep := reportFor(t, resp, "tasks")
	if rep.Timeout == nil || rep.Timeout.Budget != recall.TimeoutSourceLimit ||
		rep.Timeout.Limit != 20*time.Millisecond {
		t.Fatalf("timeout after later caller expiry = %+v, want source timeout", rep.Timeout)
	}
}

// Source content is data. Control sequences must not survive into anything a
// terminal renders.
func TestRetrievedContentIsSanitized(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{
			cand("docs", "a.md", 1, func(c *recall.Candidate) {
				c.Title = "before\x1b[31mred"
				c.Excerpt = "click javascript:alert(1)"
			}),
		}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("no results")
	}
	got := resp.Results[0].Primary
	if strings.ContainsRune(got.Title, 0x1b) {
		t.Errorf("title still carries an escape: %q", got.Title)
	}
	if strings.Contains(got.Excerpt, "javascript:") {
		t.Errorf("excerpt still carries an executable scheme: %q", got.Excerpt)
	}
}

// A health probe is a round trip: an index to open, a server to reach, a
// process to spawn. The plan already made one to decide eligibility, and the
// reporting that used to make a second one restated the same report from a
// later instant. For the td adapter that second probe was two of the eight
// process spawns one query cost, each reading a whole workspace.
func TestOneHealthProbePerSourcePerQuery(t *testing.T) {
	h := newHarness(t, nil)

	if _, err := h.app.Query(context.Background(), query("anything")); err != nil {
		t.Fatal(err)
	}
	probed := 0
	for name, f := range h.fakes {
		if f.healthCalls > 1 {
			t.Errorf("%s was probed %d times for one query", name, f.healthCalls)
		}
		probed += f.healthCalls
	}
	if probed == 0 {
		t.Fatal("no source was probed at all; the assertion above would pass vacuously")
	}
}

func TestPreparedHealthStateReachesOnlyThatRequestsSearch(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].prepared = true
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	docs := h.fakes["fakedocs"]
	if docs.prepareCalls != 1 || docs.preparedSearchCalls != 1 || !docs.sawOwnPreparation {
		t.Fatalf("prepare/search calls = %d/%d own=%v, want one request-scoped handoff",
			docs.prepareCalls, docs.preparedSearchCalls, docs.sawOwnPreparation)
	}
	if docs.healthCalls != 1 {
		t.Fatalf("health calls = %d, want preparation to be the one eligibility probe", docs.healthCalls)
	}
	if len(resp.Results) == 0 || resp.Results[0].Primary.SourceUID != "01UIDDOCS" {
		t.Fatalf("prepared path bypassed identity/ranking: %+v", resp.Results)
	}
	for name, f := range h.fakes {
		if name != "fakedocs" && (f.prepareCalls != 0 || f.preparedSearchCalls != 0) {
			t.Errorf("%s received another source's preparation", name)
		}
	}
}

// Freshness is a source-health fact that ranking cannot know, so the app fills
// it. Without this the freshness block is missing from every explanation, and
// those are the fields that say whether an answer is current.
func TestExplanationCarriesFreshness(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
		f["fakedocs"].health = recall.Health{
			Status:          recall.HealthHealthy,
			Coverage:        recall.IndexComplete,
			IndexGeneration: "gen-000042",
			IndexConfig:     "bm25-k1.2",
		}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("no results")
	}
	fr := resp.Results[0].Explanation.Freshness
	if fr.Mode != recall.FreshnessIndexed {
		t.Errorf("freshness mode = %q, want the configured indexed", fr.Mode)
	}
	if fr.IndexGeneration != "gen-000042" || fr.IndexConfig != "bm25-k1.2" {
		t.Errorf("generation identity missing from the explanation: %+v", fr)
	}
}

// The plan is returned so a caller can see what was searched rather than
// inferring it from what came back.
func TestPlanReportsEveryDecision(t *testing.T) {
	h := newHarness(t, nil)

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range resp.Plan.Sources {
		seen[p.SourceID] = true
		if !p.Eligible && p.Reason == "" {
			t.Errorf("%s was excluded with no reason given", p.SourceID)
		}
	}
	for _, want := range []string{"docs", "tasks", "vault"} {
		if !seen[want] {
			t.Errorf("plan does not mention %q", want)
		}
	}
	if resp.Plan.RankConst == 0 {
		t.Error("plan omits the rank constant that produced the ordering")
	}
}

// A locator can be held and replayed long after the response that carried it,
// and a ceiling may have narrowed in between.
func TestExpandRechecksPermission(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakevault"].evidence = recall.ExpandResponse{Content: "the secret"}
	})

	_, err := h.app.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "vault", Local: "secret"},
		Detail:  recall.DetailFull,
	}, "work")
	if !errors.Is(err, protocol.ErrSourceDenied) {
		t.Fatalf("err = %v, want ErrSourceDenied", err)
	}
}

// A portable locator may name a source configured on another machine. That is a
// fact about this configuration, not a missing record.
func TestExpandUnknownSourceIsNotConfigured(t *testing.T) {
	h := newHarness(t, nil)

	_, err := h.app.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "jira", Local: "PROJ-1"},
	}, "work")
	if !errors.Is(err, protocol.ErrSourceNotConfigured) {
		t.Fatalf("err = %v, want ErrSourceNotConfigured", err)
	}
}

func TestExpandSanitizesEvidence(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].evidence = recall.ExpandResponse{
			Content:    "line one\x1b]0;pwned\x07line two",
			Provenance: "a.md:1-2",
		}
	})

	got, err := h.app.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "docs", Local: "a.md"},
		Detail:  recall.DetailFull,
	}, "work")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(got.Content, 0x1b) {
		t.Errorf("evidence still carries an escape: %q", got.Content)
	}
}

// A built-in adapter is constructed unconfigured: it is told where to read at
// the handshake, so it cannot answer a health probe before one. Probing first
// excluded every built-in source as unhealthy, which made `recall query` return
// nothing while `recall doctor` — which initializes before probing — called the
// same source healthy.
func TestBuiltInSourcesAreHandshakenBeforeTheyAreProbed(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		for _, adp := range f {
			adp.needsHandshake = true
		}
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "a.md", 1)}
	})

	resp, err := h.app.Query(context.Background(), query("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if rep := reportFor(t, resp, "docs"); rep.Outcome != recall.SearchSuccess {
		t.Errorf("docs outcome = %s (%s), want success: a built-in was excluded before it was configured",
			rep.Outcome, rep.Reason)
	}
	if len(resp.Results) == 0 {
		t.Error("no results: an unconfigured built-in cannot answer, and it was never configured")
	}
}

// An expansion is often the first thing a fresh process does with a locator
// somebody saved yesterday, so it cannot assume a handshake already happened.
func TestExpandHandshakesBeforeReachingTheAdapter(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		for _, adp := range f {
			adp.needsHandshake = true
		}
		f["fakedocs"].evidence = recall.ExpandResponse{Content: "the section"}
	})

	got, err := h.app.Expand(context.Background(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "docs", Local: "a.md"},
		Detail:  recall.DetailFull,
	}, "work")
	if err != nil {
		t.Fatalf("expand against an unconfigured built-in: %v", err)
	}
	if got.Content == "" {
		t.Error("no evidence returned")
	}
	if !h.fakes["fakedocs"].initialized {
		t.Error("the adapter was asked to expand before it was told where to read")
	}
}

// The limit belongs to the request, not to the Ranker. A long-lived service
// serves many callers from one Ranker and has to honor each caller's ask.
func TestPerRequestLimitIsHonored(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		var many []recall.Candidate
		for i := range 8 {
			many = append(many, cand("docs", string(rune('a'+i))+".md", i+1))
		}
		f["fakedocs"].candidates = many
	})

	req := query("anything")
	req.Limit = 3

	resp, err := h.app.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 3 {
		t.Errorf("results = %d, want the requested 3", len(resp.Results))
	}
}

// td-57b319, end to end. The two rules that decide how long an answer is are
// profile configuration, and they reach fusion through the one mapping every
// transport shares — an application test rather than a ranking one because the
// defect was never in the arithmetic. It was that nothing configured either
// rule, so a natural-language query's length was the profile's own arithmetic:
// thirteen eligible sources times twenty candidates each.
//
// The plan states both, for the reason it states the rank constant: a value
// that shortens an answer without appearing in the plan is one nobody can check.
func TestProfileVolumeRulesReachFusionAndAreReported(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		var many []recall.Candidate
		for i := range 25 {
			many = append(many, cand("docs", fmt.Sprintf("doc-%02d.md", i), i+1,
				relevance(0.9)))
		}
		// One record the source itself reports as not about the query. No
		// budget, ordering, or response ceiling removes it: it is not surplus,
		// it is an answer to a different question.
		many = append(many, cand("docs", "unrelated.md", 26, relevance(0)))
		f["fakedocs"].candidates = many
	})

	// No per-request limit: the point is what a caller who named none receives.
	req := query("anything")
	req.Limit = 0

	resp, err := h.app.Query(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(resp.Results); got != config.DefaultMaxResults {
		t.Errorf("results = %d, want the profile's budget of %d", got, config.DefaultMaxResults)
	}
	if !resp.Truncated || resp.DroppedResults == 0 {
		t.Errorf("truncated = %v, dropped = %d; a bounded answer says it was bounded",
			resp.Truncated, resp.DroppedResults)
	}
	var floored int
	for _, s := range resp.Suppressed {
		if s.Reason == recall.SuppressRelevanceFloor {
			floored += s.Count
		}
	}
	if floored != 1 {
		t.Errorf("relevance-floor suppressions = %d, want the one unrelated record", floored)
	}
	for _, r := range resp.Results {
		if r.Primary.SourceRecordID == "unrelated.md" {
			t.Error("a record below the floor reached the answer")
		}
	}
	if resp.Plan.Limit != config.DefaultMaxResults ||
		resp.Plan.RelevanceFloor != config.DefaultRelevanceFloor {
		t.Errorf("plan reports limit %d and floor %v, want %d and %v",
			resp.Plan.Limit, resp.Plan.RelevanceFloor,
			config.DefaultMaxResults, config.DefaultRelevanceFloor)
	}
}

// A project filter reaches the adapters, and a project no source serves does
// not come back as complete coverage.
//
// This is the application boundary behind every host surface. A contract field
// that only adapter package tests populate is otherwise one nobody can tell is
// broken end to end.
func TestProjectScopeRoutesAndDoesNotFakeCompleteCoverage(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].project = "recall"
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "d1", 1)}
		f["faketasks"].project = "atlas-workspace"
		f["faketasks"].candidates = []recall.Candidate{cand("tasks", "t1", 1)}
	})

	t.Run("routes to the source that serves it", func(t *testing.T) {
		resp, err := h.app.Query(context.Background(), recall.QueryRequest{
			Query: "ranking", Scope: &recall.Scope{Project: "recall"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if h.fakes["fakedocs"].sawProject != "recall" {
			t.Errorf("adapter saw project %q; the filter never reached it",
				h.fakes["fakedocs"].sawProject)
		}
		if len(resp.Results) != 1 {
			t.Fatalf("results = %d, want only the source serving the named project", len(resp.Results))
		}
		// The source that is not the one named skipped, and that alone does not
		// degrade: routing working as configured is not impairment.
		if resp.Coverage != recall.CoverageComplete {
			t.Errorf("coverage = %q, want complete: a routed request is not a degraded one", resp.Coverage)
		}
	})

	t.Run("a project no source serves is not complete coverage", func(t *testing.T) {
		resp, err := h.app.Query(context.Background(), recall.QueryRequest{
			Query: "ranking", Scope: &recall.Scope{Project: "nonexistent"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Results) != 0 {
			t.Fatalf("results = %d for a project nothing serves", len(resp.Results))
		}
		// The defect this ticket is about. Every source skipped, so nothing
		// looked anywhere — and the response used to say it had looked
		// everywhere and found nothing.
		if resp.Coverage == recall.CoverageComplete {
			t.Error("coverage complete over a project no source serves: nothing searched, " +
				"so there is no boundary for `complete` to describe")
		}
		for _, r := range resp.SourceOutcomes {
			if r.Outcome == recall.SearchSuccess {
				t.Errorf("source %s reported success without searching", r.SourceID)
			}
		}
	})
}

// An adapter that skips without saying why degrades, rather than being taken
// at its word. A silent skip is indistinguishable from a boundary the request
// was free to miss, and the safe reading is the one that does not claim
// coverage nobody established.
func TestUnexplainedSkipDegrades(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].outcome = recall.SearchSkipped
		f["faketasks"].candidates = []recall.Candidate{cand("tasks", "t1", 1)}
	})
	resp, err := h.app.Query(context.Background(), recall.QueryRequest{Query: "ranking"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Coverage != recall.CoverageDegraded {
		t.Errorf("coverage = %q, want degraded for a skip with no stated reason", resp.Coverage)
	}
}

// A skipped response is a declaration that the source did not answer the
// constrained question. Candidates attached to it are necessarily broader
// evidence and must not reach fusion, even if an external adapter is broken.
func TestUnsupportedFilterCannotSmuggleCandidatesIntoFusion(t *testing.T) {
	h := newHarness(t, func(f map[string]*fake) {
		f["fakedocs"].outcome = recall.SearchSkipped
		f["fakedocs"].reason = recall.SkipFilterUnsupported
		f["fakedocs"].candidates = []recall.Candidate{cand("docs", "out-of-scope", 1)}
		f["faketasks"].candidates = []recall.Candidate{cand("tasks", "in-scope", 1)}
	})
	resp, err := h.app.Query(context.Background(), recall.QueryRequest{
		Query: "ranking", Scope: &recall.Scope{Entities: []string{"Marcus"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.fakes["fakedocs"].sawEntities; len(got) != 1 || got[0] != "Marcus" {
		t.Fatalf("adapter saw entities %v, want [Marcus]", got)
	}
	if len(resp.Results) != 1 || resp.Results[0].Primary.SourceRecordID != "in-scope" {
		t.Fatalf("results = %+v, want only in-scope evidence", resp.Results)
	}
	if resp.Coverage != recall.CoverageDegraded {
		t.Errorf("coverage = %q, want degraded for mixed success and filter_unsupported", resp.Coverage)
	}
	if rep := reportFor(t, resp, "docs"); rep.Candidates != 0 {
		t.Errorf("skipped source reports %d candidates, want 0", rep.Candidates)
	}
}
