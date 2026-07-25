package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/recall"
)

// builtins stands in for the adapters compiled into Recall. Configuration
// validates against them and never invents one.
var builtins = []config.Builtin{{
	Name:           "documents",
	FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed, recall.FreshnessHybrid},
}}

// tempPaths points Recall at a config home the test owns. HOME is set too, so
// a fixture using "~" expands somewhere hermetic rather than into the machine
// running the test.
func tempPaths(t *testing.T, configHome string) config.Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "unused"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "unused"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "unused"))
	return config.Paths{
		ConfigHome: configHome,
		StateHome:  filepath.Join(home, "state"),
		CacheHome:  filepath.Join(home, "cache"),
	}
}

func load(t *testing.T, configHome, projectFile string) (*config.Config, error) {
	t.Helper()
	return config.Load(config.Options{
		Paths:       tempPaths(t, abs(t, configHome)),
		ProjectFile: abs(t, projectFile),
		Builtins:    builtins,
	})
}

func mustLoad(t *testing.T, configHome, projectFile string) *config.Config {
	t.Helper()
	cfg, err := load(t, configHome, projectFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func abs(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	out, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeProject puts a project file in its own directory, so a relative path
// inside it has an unambiguous anchor.
// writeHome builds a user-layer config home. Identity rules live in the user
// layer now, so testing them needs a config.toml rather than a project file.
func writeHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), body)
	return home
}

func writeProject(t *testing.T, body string) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), config.ProjectFileName), body)
}

func source(t *testing.T, cfg *config.Config, id string) *config.SourceInstance {
	t.Helper()
	s, ok := cfg.Source(id)
	if !ok {
		t.Fatalf("source %q not configured", id)
	}
	return s
}

// Configuration is the resolver: it is the only place the mapping between a
// display name and an immutable identity is written down.
var _ lineage.Resolver = (*config.Config)(nil)

func TestUserLayerLoads(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "")

	if got := len(cfg.Sources); got != 3 {
		t.Fatalf("sources = %d, want 3", got)
	}
	// Ordering is by display name, so two runs over the same files agree.
	var ids []string
	for _, s := range cfg.Sources {
		ids = append(ids, s.ID)
	}
	if want := []string{"clara-docs", "clara-signals", "tasks"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", ids, want)
	}

	tasks := source(t, cfg, "tasks")
	if tasks.BasePrior != 1.4 || tasks.Timeout != 500*time.Millisecond {
		t.Errorf("tasks = %+v", tasks)
	}
	if tasks.Settings["cli"] != "td" {
		t.Errorf("settings = %v, want the adapter-owned block carried through", tasks.Settings)
	}
	// A "~" location expands against HOME, which this test owns.
	if !filepath.IsAbs(tasks.Location) || strings.HasPrefix(tasks.Location, "~") {
		t.Errorf("location = %q, want an expanded absolute path", tasks.Location)
	}

	// adapters.d is user-level and therefore trusted with a command.
	stream, ok := cfg.Adapter("stream")
	if !ok {
		t.Fatal("adapters.d did not register the stream adapter")
	}
	if stream.Command != "/usr/local/bin/recall-stream" {
		t.Errorf("command = %q", stream.Command)
	}
	if got := stream.Secrets["bearer"].EnvVar; got != "RECALL_STREAM_TOKEN" {
		t.Errorf("secret = %q, want the reference", got)
	}
}

// Project over user, field by field. What the project did not mention keeps
// the user's value and, just as importantly, keeps the user's origin.
func TestProjectLayerOverridesUser(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")

	docs := source(t, cfg, "clara-docs")
	if docs.BasePrior != 1.6 {
		t.Errorf("base_prior = %v, want the project's 1.6", docs.BasePrior)
	}
	if docs.Timeout != 900*time.Millisecond {
		t.Errorf("timeout = %v, want the project's 900ms", docs.Timeout)
	}
	if docs.UID != "01J8ZKQ4M8DOCS" {
		t.Errorf("uid = %q, want the user's identity carried through the merge", docs.UID)
	}
	if docs.Sensitivity != recall.SensitivityConfidential {
		t.Errorf("sensitivity = %s, want the user's floor untouched", docs.Sensitivity)
	}
	if origin := docs.Origin("base_prior"); origin.Layer != config.LayerProject {
		t.Errorf("base_prior origin = %s, want project", origin)
	}
	if origin := docs.Origin("sensitivity"); origin.Layer != config.LayerUser {
		t.Errorf("sensitivity origin = %s, want user", origin)
	}

	// A source the project introduces is a full source instance, not a patch.
	// Its identity is derived rather than declared: a project file may not
	// choose one, or a saved reference could be made to resolve against
	// repo-chosen data.
	notes := source(t, cfg, "repo-notes")
	if notes.Adapter != "documents" {
		t.Errorf("repo-notes = %+v", notes)
	}
	if !strings.HasPrefix(string(notes.UID), config.ProjectUIDPrefix) {
		t.Errorf("repo-notes uid = %q, want a derived %s… identity",
			notes.UID, config.ProjectUIDPrefix)
	}

	// A source the project never mentioned is untouched.
	tasks := source(t, cfg, "tasks")
	if tasks.BasePrior != 1.4 || tasks.Origin("base_prior").Layer != config.LayerUser {
		t.Errorf("tasks base_prior = %v from %s", tasks.BasePrior, tasks.Origin("base_prior"))
	}

	// A project may narrow a ceiling. The denied source stays a member: denial
	// is a reported outcome, and removing it here would be indistinguishable
	// from never having configured it.
	work, ok := cfg.Profile("work")
	if !ok {
		t.Fatal("profile work missing")
	}
	if work.MaxSensitivity != recall.SensitivityInternal {
		t.Errorf("ceiling = %s, want the project's narrower internal", work.MaxSensitivity)
	}
	if !work.Contains("clara-docs") {
		t.Error("a denied source must remain a visible member of the profile")
	}
	if work.Permits(*docs) {
		t.Error("a confidential source must not pass an internal ceiling")
	}
}

// An inherited default is resolved after every layer has spoken, not when a
// source is first read — otherwise the result would depend on which file
// happened to be parsed first.
func TestDefaultsAreInheritedAfterAllLayersMerge(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "")

	// clara-signals declares no timeout of its own, so it takes the default.
	if got := source(t, cfg, "clara-signals").Timeout; got != 1500*time.Millisecond {
		t.Errorf("inherited timeout = %v, want the user default 1.5s", got)
	}
	// tasks declares one, so nothing inherited touches it.
	if got := source(t, cfg, "tasks").Timeout; got != 500*time.Millisecond {
		t.Errorf("declared timeout = %v, want the source's own 500ms", got)
	}
}

// A project file may not raise a profile's ceiling — but selecting a *different*
// profile reaches the same end without touching one. A cloned repository that
// can point defaults.profile at a more permissive profile has escaped the
// restriction the user configured, so [defaults] is user configuration.
func TestProjectMayNotSelectTheActiveProfile(t *testing.T) {
	body := `
[defaults]
profile = "default"
`
	_, err := load(t, "testdata/home", writeProject(t, body))
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
}

// The same key is a denial-of-service lever in weaker form: a ten-minute
// default timeout applies to every source, not only the project's own.
func TestProjectMayNotSetMachineWideTimeouts(t *testing.T) {
	body := `
[defaults]
timeout_ms = 600000
`
	_, err := load(t, "testdata/home", writeProject(t, body))
	if !errors.Is(err, config.ErrTrustBoundary) {
		t.Fatalf("err = %v, want ErrTrustBoundary", err)
	}
}

// The catch-all profile is maximally permissive by construction: it has no
// ceiling and every enabled source. Synthesizing it beside profiles a user
// deliberately restricted would leave that permissive profile permanently
// reachable, which is an escalation waiting for anything able to influence
// profile selection.
func TestCatchAllProfileExistsOnlyWhenNoProfileIsConfigured(t *testing.T) {
	configured := mustLoad(t, "testdata/home", "")
	if _, ok := configured.Profile(config.DefaultProfileName); ok {
		t.Error("a catch-all profile was synthesized alongside configured profiles")
	}

	bare := mustLoad(t, t.TempDir(), "")
	if _, ok := bare.Profile(config.DefaultProfileName); !ok {
		t.Error("with nothing configured there must still be a profile to resolve")
	}
}

// A relative path means the path the author wrote, from where they wrote it.
// Resolving it against the working directory would make the same repository
// read different files depending on where Recall was invoked.
func TestRelativePathResolvesAgainstItsOwnFile(t *testing.T) {
	home := abs(t, "testdata/home")
	projectDir := abs(t, "testdata/project/ok")

	// Run from somewhere with no relationship to either configuration file.
	t.Chdir(t.TempDir())

	cfg := mustLoad(t, home, filepath.Join(projectDir, "recall.toml"))

	for _, tc := range []struct{ id, want string }{
		{"repo-notes", filepath.Join(projectDir, "notes")},
	} {
		if got := source(t, cfg, tc.id).Location; got != tc.want {
			t.Errorf("%s location = %q, want %q", tc.id, got, tc.want)
		}
	}

	// The absolute location in the user layer is left exactly as written.
	if got := source(t, cfg, "clara-signals").Location; got != "/srv/clara/data/signals.jsonl" {
		t.Errorf("absolute location = %q", got)
	}
}

// A location carrying a scheme is an endpoint or connection reference, not a
// path, and joining a directory onto it would corrupt it.
func TestEndpointLocationIsNotTreatedAsAPath(t *testing.T) {
	body := `
[[sources]]
source_id = "repo-api"
adapter = "documents"
freshness_mode = "indexed"
location = "https://notes.example/api/v1"
`
	cfg := mustLoad(t, "testdata/home", writeProject(t, body))
	if got := source(t, cfg, "repo-api").Location; got != "https://notes.example/api/v1" {
		t.Errorf("location = %q, want it left as written", got)
	}
}

func TestOpaqueLocationReachesAdapterUnchanged(t *testing.T) {
	const account = "marcus@vorwaller.net"
	body := `
[[sources]]
source_id = "mail"
adapter = "documents"
freshness_mode = "indexed"
location = "` + account + `"
`
	cfg := mustLoad(t, "testdata/home", writeProject(t, body))
	got := source(t, cfg, "mail")
	if got.Location != account {
		t.Errorf("location = %q, want opaque account unchanged", got.Location)
	}
	if got.DeclaredLocation != account || got.LocationKind != config.LocationOpaque || got.LocationRewritten {
		t.Errorf("location decision = declared %q, kind %q, rewritten %v", got.DeclaredLocation, got.LocationKind, got.LocationRewritten)
	}
}

// The whole reason source_uid exists. Renaming a source is a display change;
// every persisted reference must keep pointing at the same data.
func TestRenamePreservesTheIdentityMapping(t *testing.T) {
	const uid = recall.SourceUID("01J8ZKQ4M7TASKS")

	before := mustLoad(t, "testdata/home", "")
	after := mustLoad(t, "testdata/home-renamed", "")

	if _, ok := before.Source("work-items"); ok {
		t.Fatal("fixture problem: the rename is supposed to be the only difference")
	}

	gotUID, ok := after.UID("work-items")
	if !ok || gotUID != uid {
		t.Fatalf("UID(work-items) = %q, %v; want the identity to follow the rename", gotUID, ok)
	}
	gotID, ok := after.ID(uid)
	if !ok || gotID != "work-items" {
		t.Fatalf("ID(%s) = %q, %v; want the new display name", uid, gotID, ok)
	}
	if _, stillThere := after.UID("tasks"); stillThere {
		t.Error("the old display name must not survive the rename")
	}

	// A locator saved before the rename still resolves to the same record.
	oldRoot, err := source(t, before, "tasks").Locator("td-f62256").LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := source(t, after, "work-items").Locator("td-f62256").LineageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if oldRoot != newRoot {
		t.Fatalf("rename moved the lineage root: %q -> %q", oldRoot, newRoot)
	}

	// And the lineage layer, which knows nothing about files, agrees.
	resolved, err := lineage.Resolve(after, recall.Locator{SourceUID: uid, Local: "td-f62256"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SourceID != "work-items" {
		t.Errorf("lineage resolved to %q", resolved.SourceID)
	}
}

// A locator that was persisted keys on the immutable identity, so expansion
// resolves through the uid rather than the display name. A miss here is the
// source_not_configured case: the locator is portable, this machine simply
// does not have that source.
func TestPersistedLocatorResolvesThroughTheIdentity(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")

	got, ok := cfg.SourceByUID("01J8ZKQ4M7TASKS")
	if !ok {
		t.Fatal("a configured identity did not resolve")
	}
	if got.ID != "tasks" {
		t.Errorf("resolved to %q", got.ID)
	}
	// Provenance survives the merge, so `recall doctor` can say which file to
	// edit for a source the project only adjusted.
	if !strings.HasSuffix(got.DeclaredIn().File, "home/recall/config.toml") {
		t.Errorf("declared in %s, want the user file that introduced it", got.DeclaredIn())
	}
	if _, ok := cfg.SourceByUID("01J8ZKQ4MZNOPE"); ok {
		t.Error("an unconfigured identity resolved to something")
	}
}

// A profile's members come back in the order the profile declared, including
// the ones the ceiling will deny. Filtering them out here would make a denied
// source indistinguishable from one that was never configured, and the query
// response has to report the difference.
func TestProfileSourcesKeepDeclaredOrderAndDeniedMembers(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")

	sources, err := cfg.ProfileSources("work")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range sources {
		ids = append(ids, s.ID)
	}
	// The user's members first, in the order they were declared, then what the
	// project added. A project may add to a profile it did not build; it may
	// not decide what that profile no longer contains.
	if want := "tasks,clara-docs,clara-signals,repo-notes"; strings.Join(ids, ",") != want {
		t.Errorf("members = %v, want %s", ids, want)
	}

	// An empty name selects the configured default profile.
	byDefault, err := cfg.ProfileSources("")
	if err != nil {
		t.Fatal(err)
	}
	if len(byDefault) != len(sources) {
		t.Errorf("default profile resolved to %d sources, want the configured default", len(byDefault))
	}

	if _, err := cfg.ProfileSources("imaginary"); !errors.Is(err, config.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid for an unknown profile", err)
	}
}

// A locator naming a source this machine does not configure is not a query
// failure; it is a fact about this configuration namespace, and the resolver
// must say so rather than guess.
func TestUnconfiguredSourceIsReportedNotInvented(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "")
	if _, ok := cfg.UID("jira"); ok {
		t.Error("resolver invented an identity for an unconfigured source")
	}
	if _, ok := cfg.ID("01J8ZKQ4MZNOPE"); ok {
		t.Error("resolver invented a name for an unknown identity")
	}
	_, err := lineage.Resolve(cfg, recall.Locator{SourceID: "jira", Local: "PROJ-1"})
	var notConfigured *lineage.ErrNotConfigured
	if !errors.As(err, &notConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// A uid is minted once, deliberately, when a source is added. Nothing may
// generate one during a load: a uid that appears by itself would differ per
// machine, and every persisted reference would stop matching.
func TestNewSourceWithoutAnIdentityFails(t *testing.T) {
	home := writeHome(t, `
[[sources]]
source_id = "notes"
adapter = "documents"
location = "/tmp/notes"
freshness_mode = "indexed"
`)
	_, err := load(t, home, "")
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "source_uid") {
		t.Errorf("message %q does not name the missing key", err)
	}
}

// Two sources sharing an identity would collapse into one lineage namespace, so
// a saved locator would expand against whichever answered first.
func TestDuplicateSourceUIDFails(t *testing.T) {
	home := writeHome(t, `
[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "tasks"
adapter = "documents"
location = "/tmp/a"
freshness_mode = "indexed"

[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "notes"
adapter = "documents"
location = "/tmp/b"
freshness_mode = "indexed"
`)
	_, err := load(t, home, "")
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{"01J8ZKQ4M7TASKS", "tasks", "notes"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not name %q", msg, want)
		}
	}
}

// The same collision inside one layer, which is where a copy-paste actually
// produces it.
func TestDuplicateSourceUIDWithinOneFileFails(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), `
[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "a"
adapter = "documents"
freshness_mode = "indexed"

[[sources]]
source_uid = "01J8ZKQ4M7TASKS"
source_id = "b"
adapter = "documents"
freshness_mode = "indexed"
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// Nothing configured is not an error: it is a machine where Recall has not
// been set up yet, and the query path reports that with its own vocabulary.
func TestAbsentConfigurationIsEmptyNotFatal(t *testing.T) {
	cfg, err := config.Load(config.Options{Paths: tempPaths(t, filepath.Join(t.TempDir(), "nothing"))})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Sources) != 0 {
		t.Errorf("sources = %d, want none", len(cfg.Sources))
	}
	// The default profile still exists, and is empty rather than absent.
	p, err := cfg.ActiveProfile("")
	if err != nil {
		t.Fatalf("default profile: %v", err)
	}
	if p.Name != config.DefaultProfileName || len(p.SourceIDs) != 0 {
		t.Errorf("profile = %+v", p)
	}
}

// A profile that nothing declared is synthesized from the enabled sources, and
// says so, so it is never mistaken for something a user wrote.
func TestSynthesizedDefaultProfileExcludesDisabledSources(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), `
[[sources]]
source_uid = "01J8ZKQ4MAAAAA"
source_id = "on"
adapter = "documents"
freshness_mode = "indexed"

[[sources]]
source_uid = "01J8ZKQ4MBBBBB"
source_id = "off"
adapter = "documents"
freshness_mode = "indexed"
enabled = false
`)
	cfg, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Profile(config.DefaultProfileName)
	if len(p.SourceIDs) != 1 || p.SourceIDs[0] != "on" {
		t.Errorf("default profile sources = %v, want only the enabled one", p.SourceIDs)
	}
	if p.Origin.Layer != config.LayerDefault {
		t.Errorf("origin = %s, want default", p.Origin)
	}
}

// An adapter defined twice has no deterministic winner, and picking one
// silently would make the resolved command depend on directory listing order.
func TestDuplicateAdapterDefinitionFails(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), `
[adapters.stream]
command = "/usr/bin/a"
freshness_modes = ["indexed"]
`)
	writeFile(t, filepath.Join(home, "recall", "adapters.d", "stream.toml"), `
[adapters.stream]
command = "/usr/bin/b"
freshness_modes = ["indexed"]
`)
	_, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins})
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// A key nobody reads is a setting its author believes is in force. Invariant 6
// leaves no room for configuration that affects nothing.
func TestUnknownKeyFails(t *testing.T) {
	_, err := load(t, "testdata/home", writeProject(t, "[[sources]]\nsource_id = \"tasks\"\nbase_piror = 1.2\n"))
	if !errors.Is(err, config.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "base_piror") {
		t.Errorf("message %q does not name the typo", err)
	}
}

func TestDiscoverProjectWalksUp(t *testing.T) {
	root := t.TempDir()
	want := writeFile(t, filepath.Join(root, config.ProjectFileName), "")
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := config.DiscoverProject(deep)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("discovered %q, want %q", got, want)
	}

	// No project file anywhere above is a normal state, not a failure: Recall
	// runs from the user layer alone.
	isolated := t.TempDir()
	got, err = config.DiscoverProject(isolated)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" && strings.HasPrefix(got, isolated) {
		t.Errorf("discovered %q under a directory with no project file", got)
	}
}

// A generated identity has to survive being written into a locator's persisted
// form, where ":" is structural, and has to pass the same validation a
// hand-written one does.
func TestGeneratedSourceUIDIsUsableAndUnique(t *testing.T) {
	seen := map[recall.SourceUID]bool{}
	var last recall.SourceUID
	for range 256 {
		uid, err := config.GenerateSourceUID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[uid] {
			t.Fatalf("generated %q twice", uid)
		}
		seen[uid] = true
		if strings.ContainsAny(string(uid), ": /\\") {
			t.Fatalf("uid %q contains a character that is structural somewhere", uid)
		}
		last = uid
	}

	home := t.TempDir()
	writeFile(t, filepath.Join(home, "recall", "config.toml"), `
[[sources]]
source_uid = "`+string(last)+`"
source_id = "s"
adapter = "documents"
freshness_mode = "indexed"
`)
	if _, err := config.Load(config.Options{Paths: tempPaths(t, home), Builtins: builtins}); err != nil {
		t.Fatalf("generated uid %q rejected by validation: %v", last, err)
	}
}
