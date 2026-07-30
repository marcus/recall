package tasks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
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
//
// Each facet has an allowlist and an exclude list, and they are not two
// spellings of one idea. "Only @home" and "everything except @work" differ
// exactly on the record that carries no context at all: the allowlist drops it,
// the exclude list keeps it. That difference is why this block grew the second
// form — see ExcludeContexts.
type settings struct {
	Binary     string   `json:"binary"`
	Scope      string   `json:"scope"`
	States     []string `json:"states"`
	Priorities []string `json:"priorities"`
	Tags       []string `json:"tags"`
	Contexts   []string `json:"contexts"`

	// The exclude lists drop a record that carries any of the named values. A
	// record carrying none of that facet is RETAINED: an exclusion that names
	// nothing the record has excludes nothing about it.
	//
	// This is the whole reason they exist. Configuring contexts = ["home"] to
	// keep work tasks out of a home profile was tried on 2026-07-24 and
	// reverted, because five of the store's records carry no context at all —
	// "Get Mike a birthday gift" among them — and the allowlist silently
	// dropped exactly the personal work it was meant to keep. A query then
	// answered `coverage complete` over a task that exists, which is a false
	// absence arriving through configuration where nothing downstream can flag
	// it. An inbox is where this bites hardest: a fresh capture has no context
	// yet, and that is the moment it matters most.
	ExcludeStates   []string `json:"exclude_states"`
	ExcludeTags     []string `json:"exclude_tags"`
	ExcludeContexts []string `json:"exclude_contexts"`

	// CompletedWeight scales the lexical score of a record in a terminal state
	// — done or cancelled. It orders within this source only; it never drops a
	// record, because "what did I decide about X" is often answered by finished
	// work.
	//
	// Unset means [DefaultCompletedWeight]. An explicit 0 is a different and
	// legal setting — score terminal records to nothing — which is exactly why
	// this is a pointer and not a float with a zero-means-default rule.
	CompletedWeight *float64 `json:"completed_weight"`

	TimeoutMS     int `json:"timeout_ms"`
	MaxCandidates int `json:"max_candidates"`
	MaxTermProbes int `json:"max_term_probes"`

	// Replay names a directory of recorded CLI output. When set, no process is
	// ever spawned: the adapter under test is the parser, the identifier
	// matching, and the ranking, over a source that cannot change underneath a
	// benchmark.
	Replay string `json:"replay"`
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
	switch {
	case runner != nil:
		// Injected by a caller in Go, which is how the package's own tests run.
	case set.Replay != "":
		// Configured replay: a committed evaluation pack cannot spawn the real
		// CLI, and a pack that could only be made deterministic from Go would
		// leave this adapter unexercised by any pack at all.
		// Relative to the source's location, not to the configuration file: the
		// recording stands in for the store, and the store is the location.
		// A source with no location falls back to the declaring file's
		// directory, which is the only other place a relative path could mean.
		dir := set.Replay
		if !filepath.IsAbs(dir) {
			base := cfg.Location
			if base == "" {
				base = cfg.BaseDir
			}
			dir = filepath.Join(base, dir)
		}
		replay, err := NewReplayRunner(dir)
		if err != nil {
			return recall.Manifest{}, err
		}
		runner = replay
	default:
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
		AsOfSupport:    recall.AsOfNone,
		RelevanceBasis: recall.RelevanceLexicalSpan,

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
// filters, and names the facet that rejected it when one did. Each facet is an
// OR within itself and an AND against the others, which is the only reading
// under which adding a filter can never widen a result set.
//
// The facet name is returned rather than logged because a filter that removes
// records has to be able to say how many: a configured filter is the one way a
// result set shrinks without any query saying so, and silence there is the same
// false-absence failure the exclude lists were added to prevent, only quieter.
func (s settings) keeps(r taskRecord) (bool, string) {
	if len(s.States) > 0 && !containsFold(s.States, r.State) {
		return false, "states"
	}
	if len(s.Priorities) > 0 && !matchesPriority(s.Priorities, r.Priority) {
		return false, "priorities"
	}
	if len(s.Tags) > 0 && !anyFold(s.Tags, r.Tags) {
		return false, "tags"
	}
	if len(s.Contexts) > 0 && !anyFold(normalizeContexts(s.Contexts), normalizeContexts(r.Contexts)) {
		return false, "contexts"
	}
	// Exclusions below. A record carrying none of the named facet falls through
	// every one of these and is kept, which is the property the allowlist did
	// not have and the reason these exist.
	if len(s.ExcludeStates) > 0 && containsFold(s.ExcludeStates, r.State) {
		return false, "exclude_states"
	}
	if len(s.ExcludeTags) > 0 && anyFold(s.ExcludeTags, r.Tags) {
		return false, "exclude_tags"
	}
	if len(s.ExcludeContexts) > 0 &&
		anyFold(normalizeContexts(s.ExcludeContexts), normalizeContexts(r.Contexts)) {
		return false, "exclude_contexts"
	}
	return true, ""
}

// DefaultCompletedWeight scales a terminal record's lexical score. A quarter is
// a demotion rather than a partition: a done task with a title hit still
// outranks an open one matched only in its body, because "when did I finish X"
// is a real question, while an open task and a done one that match equally well
// are not equally what a caller meant.
const DefaultCompletedWeight = 0.25

// completedWeight is the configured demotion, or the default when unset.
func (s settings) completedWeight() float64 {
	if s.CompletedWeight == nil {
		return DefaultCompletedWeight
	}
	return *s.CompletedWeight
}

// terminal reports whether a state means the work is over. Cancelled counts:
// the record is as finished as done, and for ranking that is the same fact.
func terminal(state string) bool {
	switch strings.ToUpper(state) {
	case "DONE", "CANCELLED":
		return true
	default:
		return false
	}
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
	for _, state := range set.ExcludeStates {
		if !knownState(state) {
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"tasks settings: unknown exclude_states state %q", state)
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
	// An allowlist and an exclude list on one facet are refused rather than
	// resolved. Both orderings are defensible — intersect them, or let the
	// narrower win — and a reader of the config cannot tell which was meant, so
	// the honest answer is to make the author say it with one form.
	for _, pair := range []struct {
		facet          string
		allow, exclude []string
	}{
		{"states", set.States, set.ExcludeStates},
		{"tags", set.Tags, set.ExcludeTags},
		{"contexts", set.Contexts, set.ExcludeContexts},
	} {
		if len(pair.allow) > 0 && len(pair.exclude) > 0 {
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"tasks settings: %s and exclude_%s are both set; use one form, "+
					"because %q keeps a record carrying no %s and %q drops it",
				pair.facet, pair.facet, "exclude_"+pair.facet, pair.facet, pair.facet)
		}
	}
	if w := set.CompletedWeight; w != nil && (*w < 0 || *w > 1 || math.IsNaN(*w)) {
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"tasks settings: completed_weight %v is not in [0,1]", *w)
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
			"exclude_states": merge(stringList,
				"Drop tasks in any of these states. Refused alongside 'states'."),
			"exclude_tags": merge(stringList,
				"Drop tasks carrying any of these tags. A task with no tags is kept. Refused alongside 'tags'."),
			"exclude_contexts": merge(stringList,
				"Drop tasks carrying any of these GTD contexts. A task with NO context is kept, which is how this differs from 'contexts' and why it exists. Refused alongside 'contexts'."),
			"completed_weight": map[string]any{
				"type": "number", "minimum": 0, "maximum": 1,
				"default":     DefaultCompletedWeight,
				"description": "Scales the lexical score of done and cancelled tasks, ordering them below active work of equal match quality. Never drops them; use exclude_states for that.",
			},
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
			"replay": map[string]any{
				"type":        "string",
				"description": "Directory of recorded CLI output, relative to the declaring configuration file. When set, no process is spawned and every invocation is answered from the recording, which is what lets a committed evaluation pack exercise this adapter.",
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
