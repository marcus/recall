package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// Validation rejects; it never repairs. A clamped prior or a defaulted timeout
// would mean the configuration a person reads and the behavior they get have
// quietly diverged, and every score explanation would cite a number no file
// contains.
func TestValidationRejectsRatherThanRepairs(t *testing.T) {
	tests := []struct {
		name string
		// project is a recall.toml layered over testdata/home, unless user is
		// set, in which case it is a whole user-layer config.toml. Identity,
		// adapter choice, and freshness support are user-layer facts, so those
		// cases cannot be expressed as a project overlay.
		project string
		user    bool
		wantKey string
		// wantIn are fragments the message must contain, so the error is
		// actionable without reading the source.
		wantIn []string
	}{
		{
			// ":" separates the source from the adapter-local part of a locator.
			// A name containing one makes every locator it prints ambiguous.
			name: "source_id containing the locator separator",
			user: true,
			project: `
[[sources]]
source_uid = "01J8ZKQ4MCCOLON"
source_id = "repo:notes"
adapter = "documents"
location = "/tmp/notes"
freshness_mode = "indexed"
`,
			wantKey: "sources[0].source_id",
			wantIn:  []string{"repo:notes", "locator separator"},
		},
		{
			name: "base_prior above the validated range",
			project: `
[[sources]]
source_id = "clara-docs"
base_prior = 2.5
`,
			wantKey: "sources[0].base_prior",
			wantIn:  []string{"2.5", "[0.5, 2]"},
		},
		{
			name: "base_prior below the validated range",
			project: `
[[sources]]
source_id = "clara-docs"
base_prior = 0.1
`,
			wantKey: "sources[0].base_prior",
			wantIn:  []string{"0.1"},
		},
		{
			// An intent prior is the authority applied for a named query class,
			// so it is bounded exactly like the base it replaces.
			name: "intent prior out of range",
			project: `
[[sources]]
source_id = "clara-docs"
intent_priors = { identifier_query = 3.0 }
`,
			wantKey: "sources[0].intent_priors.identifier_query",
			wantIn:  []string{"identifier_query", "3"},
		},
		{
			name: "zero timeout",
			project: `
[[sources]]
source_id = "clara-docs"
timeout_ms = 0
`,
			wantKey: "sources[0].timeout_ms",
			wantIn:  []string{"positive", "timeout, never empty success"},
		},
		{
			name: "negative timeout",
			project: `
[[sources]]
source_id = "clara-docs"
timeout_ms = -1
`,
			wantKey: "sources[0].timeout_ms",
			wantIn:  []string{"positive"},
		},
		{
			// The adapter declares what it can serve. Catching this at load is
			// the difference between a configuration error and a source that
			// looks healthy and fails at query time.
			name: "freshness mode the adapter does not support",
			user: true,
			project: `
[[sources]]
source_uid = "01J8ZKQ4MBDOCS"
source_id = "clara-docs"
adapter = "documents"
location = "/tmp/docs"
freshness_mode = "live"
`,
			wantKey: "sources[0].freshness_mode",
			wantIn:  []string{"documents", "live"},
		},
		{
			name: "freshness mode that does not exist",
			project: `
[[sources]]
source_id = "clara-docs"
freshness_mode = "eventual"
`,
			wantKey: "sources[0].freshness_mode",
			wantIn:  []string{"eventual"},
		},
		{
			name: "unregistered adapter",
			user: true,
			project: `
[[sources]]
source_uid = "01J8ZKQ4MDGHOST"
source_id = "ghost"
adapter = "not-registered"
location = "/tmp/ghost"
`,
			wantKey: "sources[0].adapter",
			wantIn:  []string{"not-registered", "not registered"},
		},
		{
			name: "profile naming a source that does not exist",
			project: `
[profiles.work]
sources = ["tasks", "imaginary"]
`,
			wantKey: "profiles.work.sources",
			wantIn:  []string{"imaginary"},
		},
		{
			// A secret is a reference. A configuration that could hold a value
			// would eventually hold one.
			name: "secret declaring both kinds of reference",
			project: `
[[sources]]
source_id = "clara-docs"

[sources.secrets.token]
env_var = "A"
keychain = "b/c"
`,
			wantKey: "sources[0].secrets.token",
			wantIn:  []string{"exactly one"},
		},
		{
			// The two volume rules are user-layer policy, so they are stated in
			// a whole user config rather than as a project overlay — a project
			// declaring [defaults] is refused before validation ever sees it.
			name: "negative result budget",
			user: true,
			project: `
[defaults]
profile = "work"
max_results = -1
`,
			wantKey: "defaults.max_results",
			wantIn:  []string{"negative", "0 is unbounded"},
		},
		{
			name: "negative request latency budget",
			user: true,
			project: `
[defaults]
profile = "work"
budget_ms = -1
`,
			wantKey: "defaults.budget_ms",
			wantIn:  []string{"negative", "engine fallback"},
		},
		{
			// Refused rather than clamped, for the reason every ranking value
			// is: a machine must not rank differently from the configuration
			// somebody reviewed. One is out of range as well, because relevance
			// is exactly 1 for a browse with no query terms and for a record
			// whose source could not report a length — so a floor of 1 keeps
			// precisely the candidates that told fusion nothing.
			name: "relevance floor at the excluded bound",
			user: true,
			project: `
[defaults]
profile = "work"
relevance_floor = 1.0
`,
			wantKey: "defaults.relevance_floor",
			wantIn:  []string{"[0, 1)", "0 admits every candidate"},
		},
		{
			name: "negative relevance floor",
			user: true,
			project: `
[defaults]
profile = "work"
relevance_floor = -0.2
`,
			wantKey: "defaults.relevance_floor",
			wantIn:  []string{"[0, 1)"},
		},
		{
			name: "secret declaring neither kind of reference",
			project: `
[[sources]]
source_id = "clara-docs"

[sources.secrets.token]
`,
			wantKey: "sources[0].secrets.token",
			wantIn:  []string{"reference"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, project := "testdata/home", ""
			if tt.user {
				home = writeHome(t, tt.project)
			} else {
				project = writeProject(t, tt.project)
			}
			cfg, err := load(t, home, project)
			if cfg != nil {
				t.Fatal("an invalid configuration must not be returned, not even partly")
			}
			if !errors.Is(err, config.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}

			var cErr *config.Error
			if !errors.As(err, &cErr) {
				t.Fatalf("err = %v, want a located *config.Error", err)
			}
			if cErr.Key != tt.wantKey {
				t.Errorf("key = %q, want %q", cErr.Key, tt.wantKey)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not contain %q", err, want)
				}
			}
		})
	}
}

// A prior at either bound is valid: the range is closed, and an evaluation
// sweeping it must be able to reach the ends.
func TestPriorBoundsAreInclusive(t *testing.T) {
	for _, prior := range []string{"0.5", "2.0", "1.0"} {
		body := "[[sources]]\nsource_id = \"clara-docs\"\nbase_prior = " + prior + "\n"
		if _, err := load(t, "testdata/home", writeProject(t, body)); err != nil {
			t.Errorf("base_prior %s rejected: %v", prior, err)
		}
	}
}

// A prior is only meaningful if something reads it, and what reads it must be
// able to say which rule fired. An adjustment nobody can attribute is exactly
// the dead configuration invariant 6 forbids.
func TestPriorIsExplainedPerQueryClass(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "")
	tasks := source(t, cfg, "tasks")

	base := tasks.Prior("something_unconfigured")
	if base.Effective != 1.4 || base.Rule != "" || base.Intent != 0 {
		t.Errorf("unconfigured class = %+v, want the base with no rule fired", base)
	}

	intent := tasks.Prior("identifier_query")
	if intent.Effective != 1.8 || intent.Rule != "identifier_query" {
		t.Errorf("identifier_query = %+v, want the configured authority and its rule", intent)
	}
	if got := intent.Base + intent.Intent; got != intent.Effective {
		t.Errorf("components %v + %v do not add up to %v", intent.Base, intent.Intent, intent.Effective)
	}
}

// An adapter that declares no freshness modes cannot be checked against, so a
// source naming any mode would be accepted and fail later, at query time.
func TestAdapterMustDeclareItsFreshnessModes(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.mystery]
command = "/usr/bin/mystery"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "freshness_modes") {
		t.Errorf("message %q does not name the missing declaration", err)
	}
}

// A source that omits the mode gets it only when the adapter leaves no choice.
// Guessing between two supported modes would decide freshness policy for the
// user without saying so.
func TestFreshnessModeIsInferredOnlyWhenUnambiguous(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.only-live]
command = "recall-live"
freshness_modes = ["live"]

[[sources]]
source_uid = "01J8ZKQ4MEONLY1"
source_id = "one-mode"
adapter = "only-live"
`)
	cfg, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if err != nil {
		t.Fatalf("a single supported mode should need no declaration: %v", err)
	}
	if got := source(t, cfg, "one-mode").FreshnessMode; got != "live" {
		t.Errorf("freshness_mode = %q, want the adapter's only mode", got)
	}

	// The built-in documents adapter supports two, so silence is ambiguous.
	ambiguous := t.TempDir()
	writeFile(t, ambiguous+"/recall/config.toml", `
[[sources]]
source_uid = "01J8ZKQ4MFTWO12"
source_id = "two-modes"
adapter = "documents"
`)
	_, err = config.Load(config.Options{Paths: tempPaths(t, ambiguous), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// A relative command would resolve against whatever directory Recall happened
// to be started in, which is a directory an attacker may control even when the
// configuration file is not one.
func TestRelativeAdapterCommandIsRejected(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.sneaky]
command = "./adapters/run.sh"
freshness_modes = ["live"]
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "absolute path or a bare name") {
		t.Errorf("message %q does not explain the rule", err)
	}
}

// A profile name becomes a directory under the state and cache homes, so a
// separator or a traversal segment would let it escape them.
func TestProfileNameCannotEscapeTheStateDirectory(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", ".."} {
		body := "[profiles.\"" + name + "\"]\nsources = []\n"
		_, err := load(t, "testdata/home", writeProject(t, body))
		if !errors.Is(err, config.ErrInvalid) {
			t.Errorf("profile %q: err = %v, want ErrInvalid", name, err)
		}
	}
}

// All problems at once. A person fixing configuration should see the list, and
// `recall doctor` renders exactly this.
func TestEveryProblemIsReportedNotJustTheFirst(t *testing.T) {
	body := `
[[sources]]
source_id = "clara-docs"
base_prior = 9.0
timeout_ms = 0
`
	_, err := load(t, "testdata/home", writeProject(t, body))
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, want := range []string{"base_prior", "timeout_ms"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not report %q", msg, want)
		}
	}
}

// adapters.d registers adapters. Accepting a source there would make the
// resolved configuration depend on a directory listing, and the merge order
// would stop being something a person can predict from the layer rules.
func TestAdaptersDirRegistersAdaptersOnly(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/adapters.d/oops.toml", `
[[sources]]
source_uid = "01J8ZKQ4MGSTRAY"
source_id = "stray"
adapter = "documents"
freshness_mode = "indexed"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestAdaptersDirMayNotNamePrivateEvaluationArtifacts(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/adapters.d/oops.toml", `
[evaluation]
development_pack = "/tmp/dev-pack"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}
