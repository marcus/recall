package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Adapter identity and defaults.
const (
	// AdapterID is this implementation's identity in manifests and reports.
	AdapterID = "recall-tasks/1"

	// DisplayName is the human-readable name. The instance's own name comes
	// from configuration as source_id; this is the adapter's.
	DisplayName = "Tasks"

	// DefaultBinary is resolved on PATH when nothing names an executable.
	DefaultBinary = "tasks"

	// DefaultTimeout bounds one CLI invocation. The CLI parses a local JSONL
	// file, so anything approaching this is a wedged process, not slow work.
	DefaultTimeout = 5 * time.Second

	// DefaultMaxCandidates caps one search's candidate list, so a large store
	// cannot flood the fusion pool. The core's own per-source limit narrows it
	// further.
	DefaultMaxCandidates = 50

	// DefaultMaxTermProbes caps the `--body` invocations one search may issue.
	// Each one is a process spawn, so query length must not set process count.
	DefaultMaxTermProbes = 4
)

// Store scopes, matching the CLI's list scope flags.
const (
	ScopeOpen     = "open"
	ScopeDone     = "done"
	ScopeArchived = "archived"
	ScopeAll      = "all"
)

// Options configure the adapter before any handshake.
//
// Everything here is either a test seam or a bound. Instance policy — which
// store, which states, which tags — arrives at [Adapter.Initialize] in the
// configured settings block, because that is where the spec puts it.
type Options struct {
	// Runner executes the CLI. A nil Runner is replaced at Initialize with an
	// [ExecRunner] pointed at the configured binary and store.
	Runner Runner

	// Clock supplies observation timestamps. Nil means [time.Now].
	Clock func() time.Time
}

// settings is the adapter-owned settings block, validated against
// [Manifest.SettingsSchema].
//
// Contexts and Tags are separate because the CLI keeps them separate: a
// context is a GTD `@` handle, a tag is everything else, and collapsing them
// would make "@work" and "work" the same filter.
type settings struct {
	Binary        string   `json:"binary"`
	Scope         string   `json:"scope"`
	States        []string `json:"states"`
	Priorities    []string `json:"priorities"`
	Tags          []string `json:"tags"`
	Contexts      []string `json:"contexts"`
	TimeoutMS     int      `json:"timeout_ms"`
	MaxCandidates int      `json:"max_candidates"`
	MaxTermProbes int      `json:"max_term_probes"`
}

// Adapter is the built-in Tasks source.
//
// The zero value is not usable; build one with [New]. It holds no process:
// the CLI is one-shot, so there is nothing to pool and [Adapter.Close] has
// only to make later use an error rather than a silent empty answer.
type Adapter struct {
	opts Options

	mu       sync.RWMutex
	ready    bool
	closed   bool
	sourceID string
	runner   Runner
	set      settings
	timeout  time.Duration
	floor    recall.Sensitivity
}

// New returns an uninitialized Tasks adapter.
func New(opts Options) *Adapter { return &Adapter{opts: opts} }

// ErrNotInitialized reports use before a successful handshake. It is not a
// source failure: nothing has been configured to fail yet.
var ErrNotInitialized = fmt.Errorf("%w: tasks adapter not initialized", protocol.ErrSourceUnavailable)

// Initialize negotiates the protocol version, validates the settings block,
// and resolves the store the CLI will read.
func (a *Adapter) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version := min(protocol.MaxVersion, cfg.ProtocolVersionMax)
	if err := protocol.CheckNegotiated(cfg, version); err != nil {
		return recall.Manifest{}, err
	}
	if version < protocol.MinVersion {
		// The requested range lies entirely below what this build speaks.
		// Failing here is the contract: degrading to a version neither side
		// implements is what the handshake exists to prevent.
		return recall.Manifest{}, &protocol.VersionError{
			Min: cfg.ProtocolVersionMin, Max: cfg.ProtocolVersionMax, Offered: version,
			SupportedMin: protocol.MinVersion, SupportedMax: protocol.MaxVersion,
		}
	}

	set, err := parseSettings(cfg.Settings)
	if err != nil {
		return recall.Manifest{}, err
	}

	runner := a.opts.Runner
	if runner == nil {
		runner = ExecRunner{Binary: set.binary(), Env: storeEnv(cfg.Location)}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.sourceID = cfg.SourceID
	a.runner = runner
	a.set = set
	a.timeout = set.timeout()
	a.floor = recall.SensitivityInternal
	a.ready = true

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,
		RecordTypes:     []recall.RecordType{recall.RecordTask},
		QueryModes: []recall.QueryMode{
			recall.QueryExact, recall.QueryLexical, recall.QueryStructured,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},

		// The CLI publishes no creation, revision, or observation timestamp,
		// so state at a past instant cannot be reconstructed or even filtered
		// for. Declaring `filter` over deadline or scheduled would answer a
		// historical question from current state. See the package doc.
		AsOfSupport: recall.AsOfNone,

		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		FreshnessPolicy: "live: every search reads the store through the Tasks CLI; no index, no cache",
		Sensitivity:     recall.SensitivityInternal,
		SettingsSchema:  settingsSchema(),
	}, nil
}

// session returns everything one invocation needs, or the reason there is no
// session. Holding the read lock for the whole of a search would serialize
// searches for no benefit, so state is snapshotted here instead.
func (a *Adapter) session() (Runner, time.Duration, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.closed:
		return nil, 0, adapter.ErrClosed
	case !a.ready:
		return nil, 0, ErrNotInitialized
	}
	return a.runner, a.timeout, nil
}

func (a *Adapter) config() (settings, string, recall.Sensitivity) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set, a.sourceID, a.floor
}

func (a *Adapter) now() time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	return time.Now().UTC()
}

// Close releases the adapter. There is no process to stop, but later calls
// must fail rather than answer, so the flag matters.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.ready = false
	a.runner = nil
	return nil
}

// Health probes the store.
//
// `check` is the CLI's own structural validation and is the honest health
// question for this source: not "did a command return" but "is the store
// readable and well-formed". The record count and watermark come from the same
// full listing a search reads, so health and search agree about what exists.
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, _, _ := a.config()
	checked := a.now()

	listArgs := listArgs(set.Scope, nil)
	res, err := a.run(ctx, listArgs...)
	if err != nil {
		return adapter.Unhealthy(err), nil
	}
	var records []taskRecord
	if err := decodeJSON(res, &records, listArgs...); err != nil {
		return adapter.Unhealthy(err), nil
	}

	health := recall.Health{
		Status:          recall.HealthHealthy,
		CheckedAt:       checked,
		LastSuccess:     &checked,
		SourceWatermark: watermark(res.Stdout),
		RecordCount:     int64(len(records)),
		Coverage:        recall.IndexComplete,
		Diagnostics: map[string]any{
			"scope":       set.Scope,
			"cli_wall_ms": res.Elapsed.Milliseconds(),
		},
	}

	report, verdict := a.structuralCheck(ctx)
	switch {
	case verdict != "":
		// The listing succeeded, so the source is reachable; only the
		// structural verdict is missing. Coverage drops to unknown because
		// nothing has confirmed the store is complete.
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexUnknown
		health.Diagnostics["check"] = verdict

	case !report.OK:
		// The store parsed well enough to list but failed validation, so what
		// was listed may not be all of it.
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexPartial
		health.Diagnostics["check"] = "failed"
		health.FailedCount = int64(len(report.Errors))

	case len(report.Warnings) > 0:
		health.Status = recall.HealthDegraded
		health.Diagnostics["check_warnings"] = len(report.Warnings)
	}
	return health, nil
}

// structuralCheck runs the CLI's own validation. The second return is empty
// when a verdict was obtained, and otherwise names why there is none — a
// missing verdict is not the same fact as a failing one.
func (a *Adapter) structuralCheck(ctx context.Context) (checkReport, string) {
	args := []string{"check", "--json", "--all-files"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return checkReport{}, "unavailable"
	}
	var report checkReport
	if err := decodeJSON(res, &report, args...); err != nil {
		return checkReport{}, "unreadable"
	}
	return report, ""
}

// watermark fingerprints the exact bytes a search read.
//
// The CLI publishes no store revision, so there is nothing to quote. A hash of
// the payload is still real freshness evidence: identical content means the
// same revision was searched, and any change to the store changes it.
func watermark(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// listArgs builds a `tasks list` invocation. `extra` carries the `/text`
// filters, which are the only place query text is ever allowed.
func listArgs(scope string, extra []string) []string {
	args := []string{"list", "--json"}
	switch scope {
	case ScopeDone:
		args = append(args, "--done")
	case ScopeArchived:
		args = append(args, "--archived")
	case ScopeOpen:
		args = append(args, "--open")
	default:
		args = append(args, "--all")
	}
	return append(args, extra...)
}

func (s settings) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return DefaultBinary
}

func (s settings) timeout() time.Duration {
	if s.TimeoutMS > 0 {
		return time.Duration(s.TimeoutMS) * time.Millisecond
	}
	return DefaultTimeout
}

func (s settings) maxCandidates() int {
	if s.MaxCandidates > 0 {
		return s.MaxCandidates
	}
	return DefaultMaxCandidates
}

func (s settings) maxTermProbes() int {
	if s.MaxTermProbes > 0 {
		return s.MaxTermProbes
	}
	return DefaultMaxTermProbes
}

// keeps reports whether a record passes the instance's configured field
// filters. Each facet is an OR within itself and an AND against the others,
// which is the only reading under which adding a filter can never widen a
// result set.
func (s settings) keeps(r taskRecord) bool {
	if len(s.States) > 0 && !containsFold(s.States, r.State) {
		return false
	}
	if len(s.Priorities) > 0 && !matchesPriority(s.Priorities, r.Priority) {
		return false
	}
	if len(s.Tags) > 0 && !anyFold(s.Tags, r.Tags) {
		return false
	}
	if len(s.Contexts) > 0 && !anyFold(normalizeContexts(s.Contexts), normalizeContexts(r.Contexts)) {
		return false
	}
	return true
}

func matchesPriority(want []string, got *string) bool {
	if got == nil {
		return containsFold(want, "none")
	}
	return containsFold(want, *got)
}

func containsFold(haystack []string, needle string) bool {
	return slices.ContainsFunc(haystack, func(s string) bool { return strings.EqualFold(s, needle) })
}

func anyFold(want, got []string) bool {
	return slices.ContainsFunc(got, func(g string) bool { return containsFold(want, g) })
}

// normalizeContexts drops the "@" sigil so a configured "work" and a stored
// "@work" are the same context.
func normalizeContexts(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimPrefix(s, "@")
	}
	return out
}

// parseSettings decodes and validates the adapter-owned settings block.
//
// Unknown keys are rejected. A misspelled setting that silently did nothing
// would be configuration with no code path behind it, which docs/spec.md calls
// a defect rather than a tolerance.
func parseSettings(raw map[string]any) (settings, error) {
	set := settings{Scope: ScopeAll}
	if len(raw) == 0 {
		return set, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams, "tasks settings: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams, "tasks settings: %v", err)
	}
	if set.Scope == "" {
		set.Scope = ScopeAll
	}
	switch set.Scope {
	case ScopeOpen, ScopeDone, ScopeArchived, ScopeAll:
	default:
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"tasks settings: scope %q is not one of open, done, archived, all", set.Scope)
	}
	for _, state := range set.States {
		if !knownState(state) {
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"tasks settings: unknown state %q", state)
		}
	}
	for _, priority := range set.Priorities {
		switch strings.ToUpper(priority) {
		case "A", "B", "C", "NONE":
		default:
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"tasks settings: priority %q is not one of A, B, C, none", priority)
		}
	}
	return set, nil
}

func knownState(state string) bool {
	switch strings.ToUpper(state) {
	case "INBOX", "TODO", "NEXT", "WAITING", "DONE", "CANCELLED":
		return true
	default:
		return false
	}
}

// storeEnv points the CLI at the configured store.
//
// The CLI resolves its own files through env vars, a config file, and finally
// its repository — so an instance's `location` has to become an env var rather
// than an argument. A path ending in .jsonl names the live file; anything else
// names the directory holding both files.
func storeEnv(location string) []string {
	location = expandHome(strings.TrimSpace(location))
	if location == "" {
		return nil
	}
	if strings.HasSuffix(location, ".jsonl") {
		return append(os.Environ(), "TASKS_FILE="+location)
	}
	return append(os.Environ(), "TASKS_DIR="+location)
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// settingsSchema declares the settings block. `recall doctor` validates a
// configuration against it without starting a query, so every key here is one
// the code above actually reads.
func settingsSchema() map[string]any {
	stringList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"binary": map[string]any{
				"type":        "string",
				"description": "Path to the tasks executable. Resolved on PATH when omitted.",
			},
			"scope": map[string]any{
				"type":        "string",
				"enum":        []any{ScopeOpen, ScopeDone, ScopeArchived, ScopeAll},
				"default":     ScopeAll,
				"description": "Lifecycle scope searched. 'all' includes done and archived work.",
			},
			"states":     merge(stringList, "Restrict to these task states."),
			"priorities": merge(stringList, "Restrict to these priorities: A, B, C, or none."),
			"tags":       merge(stringList, "Restrict to tasks carrying any of these tags."),
			"contexts":   merge(stringList, "Restrict to tasks carrying any of these GTD contexts."),
			"timeout_ms": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Per-invocation timeout. Composes with the request deadline; the sooner wins.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Cap on candidates returned by one search.",
			},
			"max_term_probes": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Cap on the extra `--body` invocations one search may spawn.",
			},
		},
	}
}

func merge(schema map[string]any, description string) map[string]any {
	out := make(map[string]any, len(schema)+1)
	for k, v := range schema {
		out[k] = v
	}
	out["description"] = description
	return out
}

// Refresh is a no-op that reports current health.
//
// This source is live: it holds no projection, so there is nothing to bring up
// to date. The method exists because the contract has it, and returning health
// unchanged is the honest answer — reporting success for work never done would
// let a caller believe a stale index had been rebuilt.
func (a *Adapter) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	return a.Health(ctx)
}
