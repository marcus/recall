package source

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/recall"
)

// A source that is configured, enabled, and visible in `recall sources` but not
// a member of the resolved profile is a request that cannot be served as
// written. The observed failure was the opposite: `outcome abstained  coverage
// complete  elapsed 0s` over a store holding exactly one record on the subject.
func TestScopeNamingOnlySourcesOutsideTheProfileIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)

	_, err := r.scopedOutOfProfile(scopeRequest("memory-only"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if !errors.Is(err, recall.ErrUnsatisfiableScope) {
		t.Fatalf("error = %v, want an unsatisfiable scope", err)
	}
	for _, want := range []string{"memory-only", `profile "work"`, "personal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err, want)
		}
	}
}

// An id no source answers to is refused too, and says so differently: there is
// no profile to suggest, and the caller's recourse is the source listing.
func TestScopeNamingAnUnconfiguredSourceIsRefused(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)

	_, err := r.scopedOutOfProfile(scopeRequest("nope"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if !errors.Is(err, recall.ErrUnsatisfiableScope) {
		t.Fatalf("error = %v, want an unsatisfiable scope", err)
	}
	if !strings.Contains(err.Error(), "no such source is configured") {
		t.Errorf("message %q does not say the source is unconfigured", err)
	}
}

// Partial overlap answers from what the profile has and reports the rest, which
// degrades coverage. Refusing here would withhold evidence the caller can have;
// staying silent would present a partial answer as a whole one.
func TestScopeOverlappingTheProfileDegradesRatherThanRefusing(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)

	reports, err := r.scopedOutOfProfile(
		scopeRequest("docs", "memory-only"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if err != nil {
		t.Fatalf("partial overlap refused: %v", err)
	}
	if len(reports) != 1 || reports[0].SourceID != "memory-only" {
		t.Fatalf("reports = %+v, want the out-of-profile source named", reports)
	}
	if reports[0].Reason != ReasonOutOfProfile || !Degrades(reports[0].Reason) {
		t.Errorf("reason %q does not degrade coverage", reports[0].Reason)
	}
	if got := DegradedReports(reports); len(got) != 1 || !strings.Contains(got[0], "memory-only") {
		t.Errorf("degraded reports = %v, want the excluded source named", got)
	}
}

// An unscoped request, and one scoped entirely inside the profile, are
// untouched: this rule exists for a scope that reaches outside, and must not
// make a well-formed one report anything.
func TestScopeInsideTheProfileReportsNothing(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)
	profile, instances := profileOf(t, r, "work"), profileSources(t, r, "work")

	for _, req := range []recall.QueryRequest{{Query: "x"}, scopeRequest("docs", "tasks")} {
		reports, err := r.scopedOutOfProfile(req, profile, instances)
		if err != nil || len(reports) != 0 {
			t.Errorf("scope %+v reported %+v, %v; want nothing", req.Scope, reports, err)
		}
	}
}

// A source named twice is one source: a repeated scope entry must not put the
// same name in the degraded-coverage line twice.
func TestARepeatedScopeEntryIsReportedOnce(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)

	reports, err := r.scopedOutOfProfile(
		scopeRequest("docs", "memory-only", "memory-only"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if err != nil {
		t.Fatalf("partial overlap refused: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want the source named once", reports)
	}
}

func scopeRequest(sources ...string) recall.QueryRequest {
	return recall.QueryRequest{Query: "x", Scope: &recall.Scope{SourceIDs: sources}}
}

const scopeConfigTOML = `
[defaults]
profile = "work"

[[sources]]
source_uid = "01UIDDOCS"
source_id = "docs"
adapter = "fakedocs"
location = "/tmp/docs"
sensitivity = "internal"

[[sources]]
source_uid = "01UIDTASKS"
source_id = "tasks"
adapter = "fakedocs"
location = "/tmp/tasks"
sensitivity = "internal"

[[sources]]
source_uid = "01UIDMEM"
source_id = "memory-only"
adapter = "fakedocs"
location = "/tmp/memory"
sensitivity = "internal"

[profiles.work]
sources = ["docs", "tasks"]
max_sensitivity = "internal"

[profiles.personal]
sources = ["docs", "memory-only"]
max_sensitivity = "internal"
`

// scopeRegistry builds a registry over a configuration with one source that is
// configured and enabled but outside the default profile — which is the whole
// shape of the reported bug.
func scopeRegistry(t *testing.T) (*Registry, *config.Config) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "recall")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(scopeConfigTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{
		Paths: config.Paths{
			ConfigHome: home,
			StateHome:  filepath.Join(home, "state"),
			CacheHome:  filepath.Join(home, "cache"),
		},
		Builtins: []config.Builtin{{
			Name:           "fakedocs",
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		}},
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return NewRegistry(cfg, Options{StateDir: t.TempDir()}), cfg
}

func profileOf(t *testing.T, r *Registry, name string) config.Profile {
	t.Helper()
	p, err := r.cfg.ActiveProfile(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func profileSources(t *testing.T, r *Registry, name string) []*config.SourceInstance {
	t.Helper()
	instances, err := r.cfg.ProfileSources(name)
	if err != nil {
		t.Fatal(err)
	}
	return instances
}

// An id no source answers to is refused however much else was named. There is
// nothing to put in the ledger for it — no uid, no instance — so answering the
// rest would report complete coverage over a request that named something this
// machine does not have.
func TestAnUnconfiguredSourceIsRefusedEvenBesideAMember(t *testing.T) {
	t.Parallel()
	r, _ := scopeRegistry(t)

	_, err := r.scopedOutOfProfile(
		scopeRequest("docs", "typo"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if !errors.Is(err, recall.ErrUnsatisfiableScope) {
		t.Fatalf("error = %v, want an unsatisfiable scope", err)
	}
	if !strings.Contains(err.Error(), "typo") || strings.Contains(err.Error(), "docs") {
		t.Errorf("message %q should name the id that does not resolve, and only it", err)
	}

	// And beside an out-of-profile one, which names both problems.
	_, err = r.scopedOutOfProfile(
		scopeRequest("docs", "memory-only", "typo"), profileOf(t, r, "work"), profileSources(t, r, "work"))
	if !errors.Is(err, recall.ErrUnsatisfiableScope) {
		t.Fatalf("error = %v, want an unsatisfiable scope", err)
	}
	for _, want := range []string{"typo", "memory-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err, want)
		}
	}
}
