package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// fake is a scriptable adapter. Every way a real source misbehaves is a field
// here rather than a separate type, so a test reads as the situation it models.
//
// The CLI tests use it in place of the compiled-in adapters wherever the
// situation under test is about the transport — exit codes, coverage, output
// parity — so no test depends on a corpus on disk, on the real Tasks binary, or
// on how long an index build takes.
type fake struct {
	manifest   recall.Manifest
	health     recall.Health
	healthErr  error
	candidates []recall.Candidate
	outcome    recall.SearchOutcome
	searchErr  error
	evidence   recall.ExpandResponse
	expandErr  error
	lastSearch recall.SearchRequest
}

func (f *fake) Initialize(context.Context, adapter.Config) (recall.Manifest, error) {
	if f.healthErr != nil {
		return recall.Manifest{}, f.healthErr
	}
	return f.manifest, nil
}

func (f *fake) Search(_ context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	f.lastSearch = req
	if f.searchErr != nil {
		return recall.SearchResponse{Outcome: recall.SearchUnavailable}, f.searchErr
	}
	out := f.outcome
	if out == "" {
		out = recall.SearchSuccess
	}
	return recall.SearchResponse{
		Candidates:      f.candidates,
		Outcome:         out,
		SourceWatermark: f.health.SourceWatermark,
	}, nil
}

func (f *fake) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	return f.evidence, f.expandErr
}

func (f *fake) Health(context.Context) (recall.Health, error) {
	if f.healthErr != nil {
		return recall.Health{Status: recall.HealthUnavailable}, f.healthErr
	}
	h := f.health
	if h.Status == "" {
		h.Status = recall.HealthHealthy
		h.Coverage = recall.IndexComplete
	}
	return h, nil
}

func (f *fake) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	return f.Health(ctx)
}

func (f *fake) Close() error { return nil }

func manifest() recall.Manifest {
	return recall.Manifest{
		ProtocolVersion: 1,
		AdapterID:       "fake/1",
		DisplayName:     "Fake",
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		QueryModes:      []recall.QueryMode{recall.QueryLexical},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessIndexed},
		AsOfSupport:     recall.AsOfFilter,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
	}
}

func candidate(local string, rank int, opts ...func(*recall.Candidate)) recall.Candidate {
	c := recall.Candidate{
		CandidateID:    local + "#1",
		SourceRecordID: local,
		Locator:        recall.Locator{Local: local},
		RecordType:     recall.RecordDocument,
		Title:          "title " + local,
		Excerpt:        "excerpt for " + local,
		LocalRank:      rank,
		MatchSignals:   []recall.MatchSignal{recall.MatchLexical},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

// harness is one whole machine: a config home, a state home, a cache home, a
// working directory with no project file, a pinned clock, and adapters the test
// wrote. Nothing here can reach the user's real configuration, and the fixed
// clock is what lets a golden file be a diff of formatting rather than timing.
type harness struct {
	t      *testing.T
	env    cli.Env
	stdout bytes.Buffer
	stderr bytes.Buffer

	// root is the temporary machine's root, so a golden file can be written
	// with the paths that vary between runs replaced.
	root string
}

type harnessOptions struct {
	// userTOML is written to $XDG_CONFIG_HOME/recall/config.toml.
	userTOML string
	// projectTOML, when set, is written as recall.toml in the working directory.
	projectTOML string
	// adapters replaces the compiled-in set.
	adapters []cli.Adapter
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	work := filepath.Join(root, "work")
	for _, dir := range []string{filepath.Join(configHome, "recall"), work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configFile := filepath.Join(configHome, "recall", "config.toml")
	if opts.userTOML != "" {
		if err := os.WriteFile(configFile, []byte(opts.userTOML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if opts.projectTOML != "" {
		if err := os.WriteFile(filepath.Join(work, "recall.toml"), []byte(opts.projectTOML), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A clock frozen at the moment the test starts, not at a fixed date: source
	// deadlines are real wall-clock deadlines, so a pinned past would make every
	// source report a timeout instead of an answer.
	frozen := time.Now()

	h := &harness{t: t, root: root}
	h.env = cli.Env{
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		Dir:    work,
		Paths: config.Paths{
			ConfigHome: configHome,
			StateHome:  filepath.Join(root, "state"),
			CacheHome:  filepath.Join(root, "cache"),
		},
		Adapters: opts.adapters,
		Now:      func() time.Time { return frozen },
	}
	return h
}

// run executes one command and returns its exit code and both streams.
func (h *harness) run(args ...string) (int, string, string) {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	h.env.Args = args
	code := cli.Run(h.t.Context(), h.env)
	return code, h.stdout.String(), h.stderr.String()
}

// fakeAdapters registers one fake per name, all serving indexed freshness.
func fakeAdapters(fakes map[string]*fake) []cli.Adapter {
	out := make([]cli.Adapter, 0, len(fakes))
	for name, f := range fakes {
		out = append(out, cli.Adapter{
			Name:           name,
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
			New:            func() adapter.Adapter { return f },
		})
	}
	return out
}

// twoSourceTOML is the configuration most tests use: two sources of equal
// authority, one profile, nothing exotic.
const twoSourceTOML = `
[defaults]
profile = "work"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[[sources]]
source_uid = "01UIDTASKS"
source_id = "tasks"
adapter = "faketasks"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["docs", "tasks"]
max_sensitivity = "internal"
`

func contains(t *testing.T, got, want, why string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output does not contain %q: %s\n--- output ---\n%s", want, why, got)
	}
}
