package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// Two layers merge silently by design, so without an origin a user cannot tell
// a value they wrote from one a project file replaced or one this package
// defaulted. Every field carries where it came from, including the defaults.
func TestExplainNamesTheOriginOfEveryValue(t *testing.T) {
	cfg := mustLoad(t, "testdata/home", "testdata/project/ok/recall.toml")
	e := cfg.Explain()

	docs := explainedSource(t, e, "clara-docs")
	tests := []struct {
		field string
		layer config.Layer
	}{
		// Replaced by the project file.
		{"base_prior", config.LayerProject},
		{"timeout_ms", config.LayerProject},
		// Kept from the user layer. A project may tune ranking and budget, but
		// location and settings decide what data answers under this source's
		// name, so they stay where they were declared.
		{"location", config.LayerUser},
		{"sensitivity", config.LayerUser},
		{"source_uid", config.LayerUser},
		{"adapter", config.LayerUser},
		// Never declared anywhere: a default must say so rather than look like
		// something a person wrote.
		{"enabled", config.LayerDefault},
	}
	for _, tt := range tests {
		got, ok := docs.Fields[tt.field]
		if !ok {
			t.Errorf("field %q missing from the explanation", tt.field)
			continue
		}
		if got.Layer != tt.layer {
			t.Errorf("%s came from %q, want %q", tt.field, got.Layer, tt.layer)
		}
		if (got.Origin != "") != (tt.layer != config.LayerDefault) {
			t.Errorf("%s origin = %q, inconsistent with layer %q", tt.field, got.Origin, tt.layer)
		}
	}

	if origin := docs.Fields["base_prior"].Origin; !strings.HasSuffix(origin, "project/ok/recall.toml") {
		t.Errorf("base_prior origin = %q, want the project file that set it", origin)
	}

	// Per-entry origins: the user's settings and the project's coexist inside
	// one adapter-owned block, and each says where it came from.
	tasks := explainedSource(t, e, "tasks")
	if got := tasks.IntentPriors["identifier_query"]; got.Layer != config.LayerUser {
		t.Errorf("intent prior origin = %+v, want the user layer", got)
	}

	// The identity table is what makes an evaluation pack readable after a
	// rename, so it is part of the explained view.
	if len(e.Identity) != len(cfg.Sources) {
		t.Errorf("identity table has %d rows for %d sources", len(e.Identity), len(cfg.Sources))
	}

	// Every contributing file is listed, in merge order.
	if len(e.Files) != 3 {
		t.Fatalf("files = %v, want config.toml, adapters.d/stream.toml, recall.toml", e.Files)
	}
	if e.Files[len(e.Files)-1].Layer != config.LayerProject {
		t.Error("the project file must come last: it merges over everything")
	}
}

func TestExplainShowsLocationResolutionDecision(t *testing.T) {
	projectFile := writeProject(t, `
[[sources]]
source_id = "mail"
adapter = "documents"
freshness_mode = "indexed"
location = "marcus@vorwaller.net"

[[sources]]
source_id = "notes"
adapter = "documents"
freshness_mode = "indexed"
location = "./notes"
`)
	e := mustLoad(t, "testdata/home", projectFile).Explain()

	mail := explainedSource(t, e, "mail")
	if got := mail.Fields["location_original"].Value; got != "marcus@vorwaller.net" {
		t.Errorf("opaque original = %q", got)
	}
	if got := mail.Fields["location"].Value; got != "marcus@vorwaller.net" {
		t.Errorf("opaque resolved = %q", got)
	}
	if got := mail.Fields["location_kind"].Value; got != "opaque" {
		t.Errorf("opaque kind = %q", got)
	}
	if got := mail.Fields["location_rewritten"].Value; got != false {
		t.Errorf("opaque rewritten = %v", got)
	}

	notes := explainedSource(t, e, "notes")
	if got := notes.Fields["location_original"].Value; got != "./notes" {
		t.Errorf("path original = %q", got)
	}
	if got := notes.Fields["location_kind"].Value; got != "path" {
		t.Errorf("path kind = %q", got)
	}
	if got := notes.Fields["location_rewritten"].Value; got != true {
		t.Errorf("path rewritten = %v", got)
	}
	if got, _ := notes.Fields["location"].Value.(string); !strings.HasSuffix(got, "/notes") {
		t.Errorf("path resolved = %q, want absolute notes path", got)
	}
}

// Secrets are references. This package never reads an environment variable or
// a keychain, so there is no path by which a value could reach the explained
// view — and this test fails loudly if one ever appears.
func TestExplainRevealsNoSecretValue(t *testing.T) {
	const secret = "s3cr3t-value-that-must-never-be-printed"
	t.Setenv("RECALL_SIGNALS_TOKEN", secret)
	t.Setenv("RECALL_STREAM_TOKEN", secret)

	cfg := mustLoad(t, "testdata/home", "")
	out, err := json.MarshalIndent(cfg.Explain(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)

	if strings.Contains(rendered, secret) {
		t.Fatal("the explained configuration materialized a secret value")
	}
	// The reference itself must be there: a user needs to know which variable
	// to set, and a reference is not a credential.
	for _, want := range []string{"RECALL_SIGNALS_TOKEN", "RECALL_STREAM_TOKEN", "env_var:"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("explanation does not name the reference %q", want)
		}
	}

	signals := explainedSource(t, cfg.Explain(), "clara-signals")
	token, ok := signals.Secrets["api_token"]
	if !ok {
		t.Fatal("the secret reference is missing from the source view")
	}
	if got, _ := token.Value.(string); got != "env_var:RECALL_SIGNALS_TOKEN" {
		t.Errorf("secret rendered as %q, want the reference", got)
	}
}

// `recall config explain --json` is something people diff between machines and
// before and after a change. A map iterating in random order would make every
// run differ for no reason.
func TestExplainIsDeterministic(t *testing.T) {
	// One set of paths for every load, so the only thing that could vary is
	// this package's own map iteration.
	opts := config.Options{
		Paths:       tempPaths(t, abs(t, "testdata/home")),
		ProjectFile: abs(t, "testdata/project/ok/recall.toml"),
		Builtins:    builtins,
	}
	explain := func() string {
		cfg, err := config.Load(opts)
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(cfg.Explain())
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	first := explain()
	for range 8 {
		if next := explain(); next != first {
			t.Fatalf("explanation is not stable across loads:\n%s\n%s", first, next)
		}
	}
}

// The trust boundary is only useful if a user can see what it decided, so the
// command a source will actually run is shown, with the file that declared it.
func TestExplainShowsWhatWillRunAndWhoSaidSo(t *testing.T) {
	e := mustLoad(t, "testdata/home", "").Explain()

	var stream *config.AdapterView
	for i := range e.Adapters {
		if e.Adapters[i].Name == "stream" {
			stream = &e.Adapters[i]
		}
	}
	if stream == nil {
		t.Fatal("adapter stream missing from the explanation")
	}
	if stream.Command != "/usr/local/bin/recall-stream" {
		t.Errorf("command = %q", stream.Command)
	}
	if !strings.HasSuffix(stream.Origin, "adapters.d/stream.toml") {
		t.Errorf("origin = %q, want the file that declared the command", stream.Origin)
	}
	if stream.Layer != config.LayerUser {
		t.Errorf("layer = %q, want user: only a trusted layer may declare a command", stream.Layer)
	}

	// A built-in runs no command, and the distinction is visible.
	for _, a := range e.Adapters {
		if a.Name == "documents" {
			if !a.Builtin || a.Command != "" {
				t.Errorf("built-in adapter explained as %+v", a)
			}
		}
	}
}

func explainedSource(t *testing.T, e *config.Explanation, id string) config.SourceView {
	t.Helper()
	for _, s := range e.Sources {
		if s.SourceID == id {
			return s
		}
	}
	t.Fatalf("source %q missing from the explanation", id)
	return config.SourceView{}
}
