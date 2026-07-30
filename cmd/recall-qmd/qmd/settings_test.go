package qmd_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/recall"
)

// initialize is the raw handshake, so a settings test can assert the error text
// rather than a working adapter.
func initialize(t *testing.T, location string, settings map[string]any, baseDir string) error {
	t.Helper()
	a := qmd.New(qmd.Options{Runner: healthyRunner(location, "[]")})
	_, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1,
		ProtocolVersionMax: 1,
		Workdir:            t.TempDir(),
		SourceID:           "qmd",
		Location:           location,
		BaseDir:            baseDir,
		Settings:           settings,
	})
	t.Cleanup(func() { _ = a.Close() })
	return err
}

// A misspelled setting that silently did nothing is the same defect as an
// undeclared one: it will be set by someone who then believes it took effect.
func TestUnknownSettingFailsTheHandshake(t *testing.T) {
	root := corpus(t)
	err := initialize(t, root, map[string]any{"collection": "fixture", "colection": "typo"}, "")
	if err == nil || !strings.Contains(err.Error(), "colection") {
		t.Fatalf("error = %v, want the misspelled key named", err)
	}
}

// Every key in the schema has code reading it, and every key the code reads is
// in the schema. A key in one and not the other is configuration that appears to
// work and does nothing, or a setting nobody can discover.
func TestSchemaAndParserAgree(t *testing.T) {
	root := corpus(t)
	declared := map[string]bool{}
	a := qmd.New(qmd.Options{Runner: healthyRunner(root, "[]")})
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: root, Settings: map[string]any{"collection": "fixture"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RelevanceBasis != recall.RelevanceLexicalSpan {
		t.Errorf("default relevance_basis = %q, want lexical_span", manifest.RelevanceBasis)
	}
	t.Cleanup(func() { _ = a.Close() })
	props, ok := manifest.SettingsSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("settings_schema declares no properties")
	}
	for key := range props {
		declared[key] = true
	}
	for _, key := range []string{"binary", "collection", "mode", "max_candidates",
		"timeout_ms", "refresh_timeout_ms", "replay"} {
		if !declared[key] {
			t.Errorf("the parser reads %q and the schema does not declare it", key)
		}
		delete(declared, key)
	}
	for key := range declared {
		t.Errorf("the schema declares %q and no code reads it", key)
	}
	if manifest.SettingsSchema["additionalProperties"] != false {
		t.Error("the schema admits unknown keys")
	}
}

func TestManifestDeclaresTheEffectiveExecutable(t *testing.T) {
	root := corpus(t)
	a := qmd.New(qmd.Options{})
	manifest, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: root,
		Settings: map[string]any{"collection": "fixture", "binary": "/opt/tools/qmd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if got := manifest.ExecutableRequirements; len(got) != 1 ||
		got[0].Name != "qmd" || got[0].Command != "/opt/tools/qmd" {
		t.Fatalf("executable requirements = %+v", got)
	}
}

func TestRelativeExecutableIsResolvedAgainstTheCorpus(t *testing.T) {
	root := corpus(t)
	bin := filepath.Join(root, "bin", "qmd")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := qmd.New(qmd.Options{})
	manifest, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: root,
		Settings: map[string]any{"collection": "fixture", "binary": "./bin/qmd"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if got := manifest.ExecutableRequirements[0].Command; got != bin {
		t.Fatalf("declared command = %q, want %q", got, bin)
	}
}

func TestReplayManifestDeclaresNoExecutable(t *testing.T) {
	dir := replayPack(t, "[]")
	a := qmd.New(qmd.Options{})
	manifest, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1, Workdir: t.TempDir(),
		SourceID: "qmd", Location: filepath.Join(dir, qmd.ReplayCorpusDir),
		Settings: map[string]any{"collection": "fixture", "replay": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if len(manifest.ExecutableRequirements) != 0 {
		t.Fatalf("replay requirements = %+v, want none", manifest.ExecutableRequirements)
	}
}

// The collection is required and never guessed from the location's base name: a
// guessed name would search whichever corpus happened to carry it.
func TestCollectionIsRequired(t *testing.T) {
	root := corpus(t)
	if err := initialize(t, root, map[string]any{}, ""); err == nil ||
		!strings.Contains(err.Error(), "collection") {
		t.Fatalf("error = %v, want the missing collection named", err)
	}
}

func TestSettingsRejectBadValues(t *testing.T) {
	root := corpus(t)
	cases := map[string]map[string]any{
		"mode":            {"collection": "fixture", "mode": "magic"},
		"mode type":       {"collection": "fixture", "mode": 3},
		"binary type":     {"collection": "fixture", "binary": 7},
		"candidates":      {"collection": "fixture", "max_candidates": 0},
		"candidates cap":  {"collection": "fixture", "max_candidates": 5000},
		"candidates frac": {"collection": "fixture", "max_candidates": 2.5},
		"timeout":         {"collection": "fixture", "timeout_ms": 0},
		"refresh":         {"collection": "fixture", "refresh_timeout_ms": -1},
		"collection name": {"collection": "../elsewhere"},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			if err := initialize(t, root, settings, ""); err == nil {
				t.Fatalf("%v was accepted", settings)
			}
		})
	}
}

// A relative replay path resolves against the file that declared the source, not
// against the process working directory: the latter would make one configuration
// read different fixtures depending on where Recall was started.
func TestRelativeReplayNeedsBaseDir(t *testing.T) {
	root := corpus(t)
	if err := initialize(t, root, map[string]any{
		"collection": "fixture", "replay": "packs/qmd",
	}, ""); err == nil || !strings.Contains(err.Error(), "base_dir") {
		t.Fatalf("error = %v, want base_dir named", err)
	}
}

func TestReplayResolvesAgainstBaseDir(t *testing.T) {
	dir := replayPack(t, "[]")
	base := filepath.Dir(dir)
	if err := initialize(t, filepath.Join(dir, qmd.ReplayCorpusDir), map[string]any{
		"collection": "fixture", "replay": filepath.Base(dir),
	}, base); err != nil {
		t.Fatalf("initialize: %v", err)
	}
}

func TestLocationMustBeAReadableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory.md")
	if err := initialize(t, file, map[string]any{"collection": "fixture"}, ""); err == nil {
		t.Fatal("a location that is not a directory was accepted")
	}
	if err := initialize(t, "", map[string]any{"collection": "fixture"}, ""); err == nil {
		t.Fatal("an empty location was accepted")
	}
}

func TestVersionRangeRejection(t *testing.T) {
	root := corpus(t)
	a := qmd.New(qmd.Options{Runner: healthyRunner(root, "[]")})
	if _, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: 99, ProtocolVersionMax: 99, Workdir: t.TempDir(),
		SourceID: "qmd", Location: root, Settings: map[string]any{"collection": "fixture"},
	}); err == nil {
		t.Fatal("a version range this build cannot satisfy was accepted")
	}
}
