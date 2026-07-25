package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// An adapter registration may name the directory of recorded transcripts that
// `recall doctor --conformance` replays against its command. The rules are the
// command's rules, for the command's reasons.

func TestConformanceDirectoryIsCarriedThrough(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.ongoing]
command = "/usr/local/bin/recall-ongoing"
freshness_modes = ["live"]
conformance = "/srv/recall/cmd/recall-ongoing/conformance"
`)
	cfg, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	def, ok := cfg.Adapter("ongoing")
	if !ok {
		t.Fatal("the adapter did not load")
	}
	if def.Conformance != "/srv/recall/cmd/recall-ongoing/conformance" {
		t.Errorf("conformance = %q, want the declared directory", def.Conformance)
	}
	// It has to survive into the explained configuration too: the whole point
	// of the trust boundary is that a user can see what will be run and what it
	// will be checked against.
	for _, a := range cfg.Explain().Adapters {
		if a.Name == "ongoing" && a.Conformance != def.Conformance {
			t.Errorf("explained conformance = %q, want %q", a.Conformance, def.Conformance)
		}
	}
}

func TestRelativeConformanceDirectoryIsRejected(t *testing.T) {
	// Same rule as the command, for the same reason: it would resolve against
	// whatever directory Recall was started in, and a conformance run that
	// silently checked a different suite is worse than one that checked
	// nothing.
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.ongoing]
command = "/usr/local/bin/recall-ongoing"
freshness_modes = ["live"]
conformance = "cmd/recall-ongoing/conformance"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("message %q does not explain the rule", err)
	}
}

func TestABuiltinAdapterCannotDeclareAConformanceDirectory(t *testing.T) {
	// There is no process to replay a transcript against. A built-in adapter's
	// conformance suite runs in the Go test suite, and accepting the key here
	// would be configuration with no code path behind it.
	home := t.TempDir()
	writeFile(t, home+"/recall/config.toml", `
[adapters.documents]
conformance = "/srv/recall/internal/adapters/docs/conformance"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "built in") {
		t.Errorf("message %q does not explain the rule", err)
	}
}
