package cli_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/internal/config"
)

// Two layers merge silently by design, so the whole value of `config explain`
// is that a value a project file supplied is told apart from one the user wrote
// and one that was defaulted. Both surfaces have to show the origin.
func TestConfigExplainShowsEveryValuesOrigin(t *testing.T) {
	h := newHarness(t, harnessOptions{
		userTOML: secretTOML,
		projectTOML: `
[[sources]]
source_id = "docs"
base_prior = 1.4
`,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
	})

	code, human, stderr := h.run("config", "explain")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	_, machine, _ := h.run("config", "explain", "--json")

	var e config.Explanation
	if err := json.Unmarshal([]byte(machine), &e); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, machine)
	}

	prior := e.Sources[0].Fields["base_prior"]
	if prior.Layer != config.LayerProject {
		t.Fatalf("base_prior came from %s, want the project layer to have overridden it", prior.Layer)
	}
	for what, want := range map[string]string{
		"the overriding layer": string(prior.Layer),
		"the overriding file":  prior.Origin,
		"the source identity":  string(e.Sources[0].SourceUID),
		"the config file path": e.Paths.ConfigFile,
		"the state directory":  e.Paths.StateDir,
	} {
		if want == "" {
			t.Fatalf("the JSON explanation carries no %s", what)
		}
		if !strings.Contains(human, want) {
			t.Errorf("human output is missing %s (%q)\n--- human ---\n%s", what, want, human)
		}
	}
}

// A resolved configuration holds no secret material: this package never reads
// an environment variable or a keychain, and the explanation renders the
// reference. A surface that resolved one would put a credential in a terminal,
// a log, and every pasted bug report.
func TestConfigExplainRendersSecretReferencesNeverValues(t *testing.T) {
	t.Setenv("RECALL_TEST_TOKEN", "s3cr3t-value")
	h := newHarness(t, harnessOptions{
		userTOML: secretTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
	})

	for _, args := range [][]string{{"config", "explain"}, {"config", "explain", "--json"}} {
		code, stdout, _ := h.run(args...)
		if code != cli.ExitOK {
			t.Fatalf("%v exited %d", args, code)
		}
		if strings.Contains(stdout, "s3cr3t-value") {
			t.Errorf("%v printed a secret value\n%s", args, stdout)
		}
		contains(t, stdout, "env_var:RECALL_TEST_TOKEN",
			"the reference is what configuration holds, and it is what is shown")
	}
}

// An unloadable configuration has no resolved values to explain. doctor is the
// command that itemizes why, and this one says so rather than printing a
// half-resolved tree.
func TestConfigExplainRefusesAnUnloadableConfiguration(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: "[[sources]]\nsource_id = 3\n"})

	code, stdout, stderr := h.run("config", "explain")
	if code != cli.ExitError {
		t.Errorf("exit = %d, want %d\n%s", code, cli.ExitError, stdout)
	}
	if stderr == "" {
		t.Error("nothing was written to stderr about why the configuration did not load")
	}
	if stdout != "" {
		t.Errorf("a partial explanation was printed:\n%s", stdout)
	}
}

// A profile nobody configured has no resolved state. Reporting its directories
// would answer a typo with a plausible-looking path that will never hold
// anything.
func TestConfigExplainRejectsAnUnknownProfile(t *testing.T) {
	h := newHarness(t, harnessOptions{
		userTOML: secretTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
	})

	code, _, stderr := h.run("config", "explain", "--profile", "typo")
	if code != cli.ExitError {
		t.Errorf("exit = %d, want %d", code, cli.ExitError)
	}
	contains(t, stderr, "no profile named", "the error names the profiles that do exist")
}

func TestConfigRequiresASubcommand(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: twoSourceTOML})
	for _, args := range [][]string{{"config"}, {"config", "reconfigure"}} {
		code, _, stderr := h.run(args...)
		if code != cli.ExitError {
			t.Errorf("%v exited %d, want %d", args, code, cli.ExitError)
		}
		contains(t, stderr, "subcommand", "the error names what was missing")
	}
}

const secretTOML = `
[defaults]
profile = "work"

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
freshness_mode = "indexed"
base_prior = 1.0

[sources.secrets.token]
env_var = "RECALL_TEST_TOKEN"

[profiles.work]
sources = ["docs"]
`
