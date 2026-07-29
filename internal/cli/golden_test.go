package cli_test

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Human output is a product surface, not debug printing: an operator reads it,
// a script greps it, and a person deciding whether to trust an answer reads the
// coverage line. Golden files make a change to any of that a visible diff
// rather than something noticed after it ships.
func TestGoldenHumanOutput(t *testing.T) {
	tests := []struct {
		name string
		args []string
		tune func(docs, tasks *fake)
	}{
		{
			name: "query",
			args: []string{"query", "ranking"},
			tune: healthyPair,
		},
		{
			name: "query_explain",
			args: []string{"query", "--explain", "ranking"},
			tune: healthyPair,
		},
		{
			name: "query_degraded",
			args: []string{"query", "ranking"},
			tune: func(docs, tasks *fake) {
				healthyPair(docs, tasks)
				tasks.candidates = nil
				tasks.searchErr = protocol.ErrSourceUnavailable
			},
		},
		{
			name: "query_abstained",
			args: []string{"query", "nothing here"},
			tune: func(*fake, *fake) {},
		},
		{
			name: "sources",
			args: []string{"sources"},
			tune: healthyPair,
		},
		{
			name: "doctor",
			args: []string{"doctor"},
			tune: healthyPair,
		},
		{
			name: "config_explain",
			args: []string{"config", "explain"},
			tune: healthyPair,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			docs, tasks := &fake{manifest: manifest()}, &fake{manifest: manifest()}
			tc.tune(docs, tasks)
			h := newHarness(t, harnessOptions{
				userTOML: goldenTOML,
				adapters: fakeAdapters(map[string]*fake{"fakedocs": docs, "faketasks": tasks}),
			})
			// The corpus locations have to exist: doctor checks that an eligible
			// source's path is readable, and a golden file of a machine with two
			// missing directories would only ever exercise the failure path.
			for _, dir := range []string{"docs", "tasks"} {
				if err := os.MkdirAll(filepath.Join(h.root, "config", "recall", dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			_, stdout, stderr := h.run(tc.args...)
			if stderr != "" {
				t.Fatalf("unexpected stderr: %s", stderr)
			}
			compareGolden(t, tc.name, normalize(h, stdout))
		})
	}
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run: go test ./internal/cli -update)", err)
	}
	if got != string(want) {
		t.Errorf("output changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// normalize removes the two things that cannot be identical between runs: the
// temporary machine's paths, and the wall-clock values derived from when the
// test happened to start. Everything else is compared byte for byte.
var (
	deadlines = regexp.MustCompile(`deadline \S+`)
	timeouts  = regexp.MustCompile(`timeout \d\S*`)
)

func normalize(h *harness, s string) string {
	s = strings.ReplaceAll(s, h.root, "<root>")
	s = deadlines.ReplaceAllString(s, "deadline <time>")
	return timeouts.ReplaceAllString(s, "timeout <dur>")
}

// healthyPair is two sources with something to say, and enough freshness
// evidence that the golden files show what a real answer carries.
func healthyPair(docs, tasks *fake) {
	docs.health = recall.Health{
		Status:          recall.HealthHealthy,
		Coverage:        recall.IndexComplete,
		SourceWatermark: "w-000123",
		IndexGeneration: "gen-000042",
		IndexConfig:     "bm25-k1.2",
		RecordCount:     12,
		IndexedCount:    12,
	}
	observed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	docs.candidates = []recall.Candidate{
		candidate("ranking.md#L1-20", 1, func(c *recall.Candidate) {
			c.Title = "Ranking"
			c.Excerpt = "Cross-source fusion uses rank, never raw scores."
			// The two excerpt kinds are set apart here deliberately: the marker
			// that tells a matched span from a record's opening is the whole of
			// what a caller has without --explain, so it is pinned.
			c.ExcerptKind = recall.ExcerptMatched
			c.ObservedAt = &observed
			c.SourceRevision = "rev-9"
		}),
		candidate("plan.md#L4-9", 2, func(c *recall.Candidate) {
			c.Title = "Retrieval plan"
			c.Excerpt = "The eligible sources, per-source limits, and budgets."
		}),
	}
	tasks.health = recall.Health{
		Status:   recall.HealthHealthy,
		Coverage: recall.IndexComplete,
	}
	tasks.candidates = []recall.Candidate{
		candidate("td-c53986", 1, func(c *recall.Candidate) {
			c.RecordType = recall.RecordTask
			c.Title = "Build the CLI"
			c.Excerpt = "query, expand, sources, doctor, config."
			c.ExcerptKind = recall.ExcerptPreview
			c.MatchSignals = []recall.MatchSignal{recall.MatchExactIdentifier}
		}),
	}
}

// goldenTOML pins every value the golden files show, including the ones that
// would otherwise be defaults, so a change to a default is a diff here.
const goldenTOML = `
[defaults]
profile = "work"
timeout_ms = 2000
fusion_reserve_ms = 25
max_results = 20
relevance_floor = 0.10

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
location = "./docs"
freshness_mode = "indexed"
freshness_policy = "rebuilt on demand"
sensitivity = "internal"
base_prior = 1.2
timeout_ms = 1500
record_types = ["document"]

[sources.intent_priors]
reference = 1.5

[[sources]]
source_uid = "01UIDTASKS"
source_id = "tasks"
adapter = "faketasks"
location = "./tasks"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs", "tasks"]
max_sensitivity = "internal"
`
