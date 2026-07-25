package tasks_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// TestManifestDeclaresWhatTheCLICanDo. The manifest is the basis for
// eligibility, so every claim in it has to be one this adapter can keep.
func TestManifestDeclaresWhatTheCLICanDo(t *testing.T) {
	a := tasks.New(tasks.Options{Runner: recordedStore(t), Clock: fixedClock})
	manifest, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// as_of is the claim that matters most. The CLI publishes no creation,
	// revision, or observation timestamp in any JSON shape, so state at a past
	// instant cannot be reconstructed or even filtered for. Declaring `filter`
	// over deadline or scheduled would let this source answer historical
	// questions from current state, which docs/spec.md forbids outright.
	if manifest.AsOfSupport != recall.AsOfNone {
		t.Errorf("as_of_support = %q, want none: the CLI exposes no record history",
			manifest.AsOfSupport)
	}
	if manifest.AsOfSupport.Honors() {
		t.Error("as_of_support claims it can honor a historical boundary")
	}

	// Live only. The adapter owns no index, so it must not offer to serve one.
	if !manifest.Supports(recall.FreshnessLive) {
		t.Error("manifest does not declare live")
	}
	for _, mode := range []recall.FreshnessMode{recall.FreshnessIndexed, recall.FreshnessHybrid} {
		if manifest.Supports(mode) {
			t.Errorf("manifest declares %q, but this adapter maintains no index", mode)
		}
	}

	if !slices.Equal(manifest.RecordTypes, []recall.RecordType{recall.RecordTask}) {
		t.Errorf("record_types = %v, want [task]", manifest.RecordTypes)
	}
	for _, mode := range []recall.QueryMode{recall.QueryExact, recall.QueryLexical, recall.QueryStructured} {
		if !slices.Contains(manifest.QueryModes, mode) {
			t.Errorf("query_modes is missing %q", mode)
		}
	}
	if slices.Contains(manifest.QueryModes, recall.QuerySemantic) {
		t.Error("query_modes claims semantic; there are no embeddings here")
	}
	if !manifest.Can(recall.CapSearch) || !manifest.Can(recall.CapExpand) {
		t.Errorf("capabilities = %v, want search and expand", manifest.Capabilities)
	}
	if manifest.Can(recall.CapCheckpoint) {
		t.Error("capabilities claims checkpoint, which belongs to an indexing adapter")
	}
	if manifest.SettingsSchema == nil {
		t.Error("no settings_schema; `recall doctor` cannot validate configuration without one")
	}
	if manifest.ProtocolVersion != protocol.MaxVersion {
		t.Errorf("protocol_version = %d, want %d", manifest.ProtocolVersion, protocol.MaxVersion)
	}
}

// TestInitializeRejectsAnUnreachableVersionRange. A handshake that cannot land
// inside the requested range fails explicitly rather than degrading to a
// version neither side implements.
func TestInitializeRejectsAnUnreachableVersionRange(t *testing.T) {
	a := tasks.New(tasks.Options{Runner: recordedStore(t)})
	_, err := a.Initialize(context.Background(), adapter.Config{
		ProtocolVersionMin: protocol.MaxVersion + 5,
		ProtocolVersionMax: protocol.MaxVersion + 9,
		SourceID:           "tasks",
	})
	var versionErr *protocol.VersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("err = %v, want a *protocol.VersionError", err)
	}
}

// TestInitializeValidatesSettings. A misspelled or out-of-range setting must
// fail the handshake, because a setting that silently did nothing would be
// configuration with no code path behind it.
func TestInitializeValidatesSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		why      string
	}{
		{
			name:     "unknown key",
			settings: map[string]any{"scoop": "open"},
			why:      "a typo that did nothing would look like a working filter",
		},
		{
			name:     "unknown scope",
			settings: map[string]any{"scope": "everything"},
			why:      "an unrecognized scope would silently fall back to a different one",
		},
		{
			name:     "unknown state",
			settings: map[string]any{"states": []any{"BLOCKED"}},
			why:      "a state the store does not use filters everything out, silently",
		},
		{
			name:     "unknown priority",
			settings: map[string]any{"priorities": []any{"urgent"}},
			why:      "the priority cookie is A, B, C, or none",
		},
		{
			name:     "wrong type",
			settings: map[string]any{"timeout_ms": "fast"},
			why:      "a string where an integer belongs is a configuration error, not a default",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := tasks.New(tasks.Options{Runner: recordedStore(t)})
			_, err := a.Initialize(context.Background(), adapter.Config{
				ProtocolVersionMin: protocol.MinVersion,
				ProtocolVersionMax: protocol.MaxVersion,
				SourceID:           "tasks",
				Settings:           tc.settings,
			})
			if err == nil {
				t.Fatalf("initialize accepted invalid settings (%s)", tc.why)
			}
		})
	}
}

// TestHealthReportsAReadableStore. Health for a live source is not "did a
// command return" but "is the store readable and well-formed", which is what
// `tasks check` answers.
func TestHealthReportsAReadableStore(t *testing.T) {
	a := newAdapter(t, recordedStore(t), nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthHealthy {
		t.Errorf("status = %q, want healthy", health.Status)
	}
	if !health.Usable() {
		t.Error("a healthy source reports itself unusable")
	}
	if health.Coverage != recall.IndexComplete {
		t.Errorf("coverage = %q, want complete: a live full listing enumerates everything",
			health.Coverage)
	}
	if health.RecordCount == 0 {
		t.Error("record_count = 0 for a store with records")
	}
	if health.SourceWatermark == "" {
		t.Error("no source_watermark")
	}
	if health.IndexGeneration != "" || health.IndexModel != "" {
		t.Error("index fields are set on an adapter that owns no index")
	}
}

// TestHealthOnAnUnreachableStore. An unreachable source is never healthy and
// never has a known coverage — the two together are what stop a stale answer
// from looking fresh.
func TestHealthOnAnUnreachableStore(t *testing.T) {
	cli := &fakeCLI{reply: func(args []string) (tasks.Result, error) {
		return tasks.Result{Stderr: []byte("No such file or directory"), ExitCode: 1}, nil
	}}
	a := newAdapter(t, cli, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthUnavailable {
		t.Errorf("status = %q, want unavailable", health.Status)
	}
	if health.Usable() {
		t.Error("an unreachable source reports itself usable")
	}
	if health.Coverage != recall.IndexUnknown {
		t.Errorf("coverage = %q, want unknown", health.Coverage)
	}
}

// TestHealthDegradesOnAStructurallyInvalidStore. The listing succeeded, so the
// source is reachable; `check` says the file is broken, so nothing may claim
// its coverage is complete.
func TestHealthDegradesOnAStructurallyInvalidStore(t *testing.T) {
	cli := recordedStore(t)
	inner := cli.reply
	cli.reply = func(args []string) (tasks.Result, error) {
		if args[0] == "check" {
			return tasks.Result{Stdout: []byte(`{"ok":false,"errors":["line 6: malformed id"],"warnings":[]}`)}, nil
		}
		return inner(args)
	}
	a := newAdapter(t, cli, nil)

	health, err := a.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Status != recall.HealthDegraded {
		t.Errorf("status = %q, want degraded", health.Status)
	}
	if health.Coverage == recall.IndexComplete {
		t.Error("coverage claims complete over a store that failed validation")
	}
	if health.FailedCount != 1 {
		t.Errorf("failed_count = %d, want 1", health.FailedCount)
	}
}

// TestCloseIsIdempotent. Close is called on teardown paths that may run twice.
func TestCloseIsIdempotent(t *testing.T) {
	a := tasks.New(tasks.Options{Runner: recordedStore(t)})
	for range 3 {
		if err := a.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// TestAdapterSatisfiesTheInterface is a compile-time claim made explicit, so
// a signature drift fails here with a readable message rather than at a call
// site in another package.
func TestAdapterSatisfiesTheInterface(t *testing.T) {
	var _ adapter.Adapter = tasks.New(tasks.Options{})
}
