package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// The security-critical case. A project configuration travels with a cloned
// repository, so a file that arrives with `git clone` must never be able to
// name a program Recall will run. Every one of these is a hard error that
// names the file and the key: a warning or a silent drop would mean the
// attacker's key was parsed and only politely ignored.
func TestProjectFileMayNotDeclareAnythingExecutable(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		wantKey string
	}{
		{
			// The obvious attempt.
			name:    "command on a source instance",
			dir:     "testdata/project/command",
			wantKey: "sources[0].command",
		},
		{
			// Buried inside the adapter-owned settings table, which this package
			// deliberately does not interpret. Scanning the raw document rather
			// than the typed shape is what catches it.
			name:    "command hidden in adapter settings",
			dir:     "testdata/project/settings-command",
			wantKey: "sources[0].settings.command",
		},
		{
			// Defining an adapter is defining what runs, so the table is refused
			// whole rather than key by key.
			name:    "adapter definition",
			dir:     "testdata/project/adapters-table",
			wantKey: "adapters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectFile := filepath.Join(tt.dir, "recall.toml")
			cfg, err := load(t, "testdata/home", projectFile)
			if cfg != nil {
				t.Fatal("a rejected configuration must not be returned even partially")
			}
			if !errors.Is(err, config.ErrTrustBoundary) {
				t.Fatalf("err = %v, want ErrTrustBoundary", err)
			}

			var cErr *config.Error
			if !errors.As(err, &cErr) {
				t.Fatalf("err = %v, want a *config.Error carrying the location", err)
			}
			if cErr.Key != tt.wantKey {
				t.Errorf("key = %q, want %q", cErr.Key, tt.wantKey)
			}
			if !strings.HasSuffix(filepath.ToSlash(cErr.File), filepath.ToSlash(projectFile)) {
				t.Errorf("file = %q, want it to name %q", cErr.File, projectFile)
			}

			// A person reads the message, not the struct: both facts must survive
			// rendering.
			msg := err.Error()
			for _, want := range []string{tt.wantKey, "recall.toml"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not name %q", msg, want)
				}
			}
		})
	}
}

// The forbidden set is matched by name anywhere in the document, so a key that
// merely looks executable fails before anyone implements it. Each case here is
// a key a plausible future adapter might have accepted.
func TestEveryExecutableKeyIsRejectedAtAnyDepth(t *testing.T) {
	for _, key := range []string{"command", "args", "argv", "env", "environment", "exec", "entrypoint", "shell"} {
		t.Run(key, func(t *testing.T) {
			body := fmt.Sprintf(`
[[sources]]
source_id = "clara-docs"
%s = "anything"
`, key)
			_, err := load(t, "testdata/home", writeProject(t, body))
			if !errors.Is(err, config.ErrTrustBoundary) {
				t.Fatalf("%s: err = %v, want ErrTrustBoundary", key, err)
			}
		})
	}
}

// Case is not a loophole: TOML keys are case-sensitive, so an adapter reading
// "Command" would be a different key, but a scanner that only matched lowercase
// would be one rename away from useless.
func TestExecutableKeyMatchIsCaseInsensitive(t *testing.T) {
	_, err := load(t, "testdata/home", writeProject(t, "[[sources]]\nsource_id = \"clara-docs\"\nCOMMAND = \"x\"\n"))
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
}

// The boundary is about the layer, not the syntax. The exact document that is
// fatal in a project file is how a user declares an adapter, and this proves
// the rejection above is not simply "Recall cannot parse this".
func TestTheSameDocumentIsValidInUserConfiguration(t *testing.T) {
	body, err := os.ReadFile("testdata/project/adapters-table/recall.toml")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), string(body)+`
[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "tasks"
adapter = "tasks"
freshness_mode = "live"
`)

	cfg, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if err != nil {
		t.Fatalf("user configuration rejected: %v", err)
	}
	adapter, ok := cfg.Adapter("tasks")
	if !ok {
		t.Fatal("adapter not registered")
	}
	if adapter.Command != "/tmp/pwn" {
		t.Errorf("command = %q, want the user's declaration honored", adapter.Command)
	}
	if adapter.Origin.Layer != config.LayerUser {
		t.Errorf("layer = %q, want user", adapter.Origin.Layer)
	}
}

// A project file may adjust a source the user declared, but the immutable
// identity belongs to the layer that created it. Reassigning it would repoint
// every saved locator and evaluation judgment that keys on the uid at whatever
// data the project chose.
func TestProjectMayNotReassignAnExistingSourceUID(t *testing.T) {
	_, err := load(t, "testdata/home", "testdata/project/reassign-uid/recall.toml")
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
	assertErrorNames(t, err, "sources[0].source_uid")
}

// Sensitivity is a floor. A project file lowering one would widen what a
// profile ceiling admits, which is access granted by a cloned repository.
func TestProjectMayNotLowerASensitivityFloor(t *testing.T) {
	_, err := load(t, "testdata/home", "testdata/project/lower-sensitivity/recall.toml")
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
	assertErrorNames(t, err, "sources[0].sensitivity")
}

// The same rule from the other side: a ceiling may be narrowed by a project,
// never widened.
func TestProjectMayNotRaiseAProfileCeiling(t *testing.T) {
	_, err := load(t, "testdata/home", "testdata/project/raise-ceiling/recall.toml")
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
	assertErrorNames(t, err, "profiles.work.max_sensitivity")
}

// A project file repointing an existing source at a different adapter changes
// which program answers for an identity the user established.
func TestProjectMayNotRepointASourceAtAnotherAdapter(t *testing.T) {
	body := `
[[sources]]
source_id = "tasks"
adapter = "documents"
`
	_, err := load(t, "testdata/home", writeProject(t, body))
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
	assertErrorNames(t, err, "sources[0].adapter")
}

func assertErrorNames(t *testing.T, err error, wantKey string) {
	t.Helper()
	var cErr *config.Error
	if !errors.As(err, &cErr) {
		t.Fatalf("err = %v, want a *config.Error", err)
	}
	if cErr.Key != wantKey {
		t.Errorf("key = %q, want %q", cErr.Key, wantKey)
	}
	if cErr.File == "" {
		t.Error("error names no file")
	}
}

// Searching a repository's own documents is a legitimate reason for a project
// file to introduce a source. Choosing that source's immutable identity is not:
// a saved locator or evaluation judgment keys on the uid, so a repo that picks
// one can make a persisted reference resolve against repo-chosen data on a
// machine where the real source is absent.
func TestProjectMayNotChooseAnIdentity(t *testing.T) {
	body := `
[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "repo-notes"
adapter = "documents"
location = "notes"
freshness_mode = "indexed"
`
	_, err := load(t, "testdata/home", writeProject(t, body))
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
}

// The source it introduces still works, under an identity Recall derived.
func TestProjectSourceGetsADerivedIdentity(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")

	notes := source(t, cfg, "repo-notes")
	if !strings.HasPrefix(string(notes.UID), config.ProjectUIDPrefix) {
		t.Errorf("uid = %q, want a derived identity", notes.UID)
	}
	// Deterministic, or every locator it prints would move between runs.
	again := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")
	if source(t, again, "repo-notes").UID != notes.UID {
		t.Error("a derived identity must be stable across loads")
	}
}

// Where a source reads from, and how its adapter is configured, decide what
// data answers under a trusted source's name. settings is the sharper of the
// two: it is adapter-owned and unvalidated at load, so a key like `cli` names a
// program without ever looking like an executable key to the trust scan.
func TestProjectMayNotRepointASourceItDoesNotOwn(t *testing.T) {
	tests := map[string]string{
		"location": `
[[sources]]
source_id = "tasks"
location = "/tmp/attacker/tasks.jsonl"
`,
		"settings": `
[[sources]]
source_id = "tasks"
[sources.settings]
cli = "/tmp/evil-td"
`,
		"enabled": `
[[sources]]
source_id = "tasks"
enabled = false
`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, "testdata/home", writeProject(t, body))
			if !errors.Is(err, config.ErrTrustBoundary) {
				t.Fatalf("err = %v, want ErrTrustBoundary", err)
			}
		})
	}
}

// A project may add its own source to a profile the user built. Replacing the
// list would let it decide what a trusted profile no longer contains, which
// hides the authoritative source for a question and leaves only its own.
func TestProjectProfileMembershipIsAdditive(t *testing.T) {
	body := `
[[sources]]
source_id = "repo-notes"
adapter = "documents"
location = "notes"
freshness_mode = "indexed"

[profiles.work]
sources = ["repo-notes"]
`
	cfg := mustLoad(t, "testdata/home", writeProject(t, body))

	work, ok := cfg.Profile("work")
	if !ok {
		t.Fatal("profile work missing")
	}
	for _, want := range []string{"tasks", "clara-docs", "repo-notes"} {
		if !work.Contains(want) {
			t.Errorf("profile lost %q; project membership must add, not replace", want)
		}
	}
}

// A settings block is adapter-owned: this package carries it and bounds nothing
// inside it, because the adapter's declared schema does that at the handshake.
// The TOML decoder reports everything under a map[string]any as undecoded, so
// rejecting those would make any nested adapter setting a configuration error.
func TestAdapterOwnedSettingsAreNotUnknownKeys(t *testing.T) {
	body := `
[[sources]]
source_id = "repo-notes"
adapter = "documents"
location = "notes"
freshness_mode = "indexed"

[sources.settings]
glob = "**/*.md"

[sources.settings.aliases]
"projects/recall/decisions.md" = ["decisions", "adr"]

[sources.settings.nested.deeper]
anything = 1
`
	cfg := mustLoad(t, "testdata/home", writeProject(t, body))
	got := source(t, cfg, "repo-notes").Settings
	if _, ok := got["aliases"]; !ok {
		t.Errorf("adapter settings were dropped: %#v", got)
	}
}

// An executable key hidden inside that same block is still refused: the trust
// scan reads the raw tree precisely because settings is where one would hide.
func TestExecutableKeyInsideSettingsIsStillRefused(t *testing.T) {
	body := `
[[sources]]
source_id = "repo-notes"
adapter = "documents"
location = "notes"
freshness_mode = "indexed"

[sources.settings.nested]
command = "/tmp/evil"
`
	if _, err := load(t, "testdata/home", writeProject(t, body)); err == nil {
		t.Fatal("an executable key nested in settings was accepted")
	}
}
