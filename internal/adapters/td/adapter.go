package td

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
	"strconv"
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
	AdapterID = "recall-td/1"

	// DisplayName is the human-readable name of the adapter. Each instance's
	// own name comes from configuration as source_id; this is the adapter's.
	DisplayName = "td"

	// DefaultBinary is resolved on PATH when nothing names an executable.
	DefaultBinary = "td"

	// DefaultTimeout bounds one td invocation. td opens a local SQLite
	// database and answers in a few hundred milliseconds on a workspace of a
	// thousand issues, so anything approaching this is a wedged process or a
	// held database lock, not slow work.
	DefaultTimeout = 5 * time.Second

	// DefaultMaxCandidates caps one search's candidate list, so a large
	// workspace cannot flood the fusion pool. The core's own per-source limit
	// narrows it further.
	DefaultMaxCandidates = 50

	// DefaultMaxTermProbes caps how many query terms are probed. Each probe is
	// a process spawn, so query length must not set process count.
	DefaultMaxTermProbes = 4

	// corpusLimit bounds the workspace listing one probe reads. It is far
	// above any workspace seen so far — the largest in this deployment holds
	// about 900 issues — and exists so a runaway workspace cannot turn one
	// search into an unbounded read. A listing that reaches the limit is
	// reported as partial coverage rather than passed off as the whole
	// workspace.
	corpusLimit = 5000
)

// Statuses and types td defines. They are validated at handshake rather than
// passed through, because a misspelled status would otherwise reach td as a
// filter that quietly matches nothing, and a source configured to see nothing
// looks exactly like a source with nothing to say.
var (
	knownStatuses = []string{"open", "in_progress", "blocked", "in_review", "closed"}
	knownTypes    = []string{"bug", "feature", "task", "epic", "chore"}
)

// Options configure the adapter before any handshake.
//
// Everything here is either a test seam or a bound. Instance policy — which
// workspace, which statuses, which labels — arrives at [Adapter.Initialize] in
// the configured settings block, because that is where the spec puts it.
type Options struct {
	// Runner executes td. A nil Runner is replaced at Initialize with an
	// [ExecRunner] pointed at the configured binary and workspace.
	Runner Runner

	// Clock supplies observation timestamps. Nil means [time.Now].
	Clock func() time.Time
}

// settings is the adapter-owned settings block, validated against
// [Manifest.SettingsSchema].
type settings struct {
	Binary string `json:"binary"`

	// Workspace asserts the identity this source expects to serve. It does not
	// override it: a value disagreeing with the workspace td resolves the
	// location to is refused at the handshake, because a name that came from
	// configuration is a name nothing checked, and locators built from one can
	// name a database this source does not read.
	Workspace string `json:"workspace"`

	Statuses []string `json:"statuses"`
	Types    []string `json:"types"`
	Labels   []string `json:"labels"`

	TimeoutMS     int `json:"timeout_ms"`
	MaxCandidates int `json:"max_candidates"`
	MaxTermProbes int `json:"max_term_probes"`

	// Replay names a directory of recorded td output. When set, no process is
	// ever spawned: the adapter under test is the parser, the identifier
	// matching, and the merge, over a workspace that cannot change underneath
	// a benchmark.
	Replay string `json:"replay"`
}

// Adapter is the built-in td source: one instance per workspace.
//
// The zero value is not usable; build one with [New]. It holds no process: td
// is a one-shot command, so there is nothing to pool and [Adapter.Close] has
// only to make later use an error rather than a silent empty answer.
type Adapter struct {
	opts Options

	mu        sync.RWMutex
	ready     bool
	closed    bool
	sourceID  string
	runner    Runner
	set       settings
	timeout   time.Duration
	floor     recall.Sensitivity
	workspace workspace
}

// New returns an uninitialized td adapter.
func New(opts Options) *Adapter { return &Adapter{opts: opts} }

// ErrNotInitialized reports use before a successful handshake. It is not a
// source failure: nothing has been configured to fail yet.
var ErrNotInitialized = fmt.Errorf("%w: td adapter not initialized", protocol.ErrSourceUnavailable)

// Initialize negotiates the protocol version, validates the settings block,
// and resolves the workspace this instance serves.
//
// It runs no td command. A workspace that is missing, unreadable, or not
// initialized must be able to report itself unavailable through [Health], and
// an instance that failed its handshake reports nothing at all.
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
	ws, err := resolveWorkspace(cfg.Location, set.Workspace, set.Replay != "")
	if err != nil {
		return recall.Manifest{}, err
	}

	runner := a.opts.Runner
	switch {
	case runner != nil:
		// Injected by a caller in Go, which is how the package's own tests run.
	case set.Replay != "":
		// Configured replay: a committed evaluation pack cannot spawn the real
		// td binary, and a pack that could only be made deterministic from Go
		// would leave this adapter unexercised by any pack at all.
		// Relative to the source's location, not to the configuration file:
		// the recording stands in for the workspace, and the workspace is the
		// location. A source with no location falls back to the declaring
		// file's directory, which is the only other place a relative path
		// could mean.
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
		// td is pointed at the CONFIGURED location, not at the root resolved
		// from it. The resolution in root.go is a mirror of td's and is allowed
		// to be wrong; sending td somewhere the mirror chose would turn a wrong
		// mirror into a wrong database, instead of into a health report saying
		// the two disagree.
		runner = ExecRunner{Binary: set.binary(), Root: ws.Location, Env: os.Environ()}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.sourceID = cfg.SourceID
	a.runner = runner
	a.set = set
	a.workspace = ws
	a.timeout = set.timeout()
	a.floor = recall.SensitivityInternal
	a.ready = true

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,

		// Epics, tasks, bugs, features, and chores are all td issues: units of
		// engineering work, which is what the core's `task` type means. td's
		// own type stays in metadata, where a caller can route on it without
		// the core having to learn td's vocabulary.
		RecordTypes: []recall.RecordType{recall.RecordTask},

		QueryModes: []recall.QueryMode{
			recall.QueryExact, recall.QueryLexical, recall.QueryStructured,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},

		// td stores a last-write timestamp, not a revision history: no prior
		// title, no prior description, no timestamps on dependency edges. An
		// issue edited after a boundary can only be reported as it is now, and
		// answering a historical question from current state is what
		// docs/spec.md forbids. See the package doc.
		AsOfSupport: recall.AsOfNone,

		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		FreshnessPolicy: "live: every search reads the workspace through the td CLI; no index, no cache",
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

func (a *Adapter) config() (settings, string, recall.Sensitivity, workspace) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set, a.sourceID, a.floor, a.workspace
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

// Health probes the workspace.
//
// The honest health question for this source is not "did a command return" but
// "does this workspace resolve to a database td can read". `td info` answers
// exactly that, and it answers it with the workspace's own name and issue
// counts, so a location pointing at the wrong repository is visible in
// diagnostics rather than silently returning another project's work.
//
// A missing, unreadable, or uninitialized workspace is unavailable. It is
// never a successful search with no matches: td exits non-zero and says
// `database not found`, and reporting that as an empty corpus would let fusion
// downstream treat "the workspace is gone" as "there is no such issue".
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, _, _, ws := a.config()
	checked := a.now()

	infoArgs := []string{"info", "--json"}
	res, err := a.run(ctx, infoArgs...)
	if err != nil {
		return adapter.Unhealthy(err), nil
	}
	var info workspaceInfo
	if err := decodeJSON(res, &info, infoArgs...); err != nil {
		return adapter.Unhealthy(err), nil
	}

	health := recall.Health{
		Status:      recall.HealthHealthy,
		CheckedAt:   checked,
		LastSuccess: &checked,
		RecordCount: info.Issues.Total,
		Coverage:    recall.IndexComplete,
		Diagnostics: map[string]any{
			"workspace":          ws.Name,
			"workspace_location": ws.Location,
			"workspace_root":     ws.Root,
			"td_project":         info.Project,
			"open":               info.Issues.Open,
			"in_progress":        info.Issues.InProgress,
			"blocked":            info.Issues.Blocked,
			"in_review":          info.Issues.InReview,
			"closed":             info.Issues.Closed,
			"cli_wall_ms":        res.Elapsed.Milliseconds(),
		},
	}
	if ws.Pinned {
		// The identity was asserted by configuration as well as resolved, and
		// the two agreed or this instance would not have finished its
		// handshake. Reported so an operator reading diagnostics can tell an
		// identity that was checked against a declaration from one that was
		// only observed.
		health.Diagnostics["workspace_asserted"] = true
	}
	switch {
	case set.Replay != "":
		// A recording is not a database. It claims no store — two sources may
		// replay one transcript without either of them reading anything — and
		// its recorded project name is whatever was recorded, so there is
		// nothing here for the identity check to compare against.
		health.Diagnostics["replay"] = true

	case !strings.EqualFold(info.Project, ws.resolvedProject()):
		// td opened a database in a directory this adapter did not predict, so
		// every locator this instance would emit names a workspace that is not
		// the one being read. Degraded rather than unavailable: the workspace
		// answers, and the records are real — it is the IDENTITY that cannot be
		// trusted, and a caller has to be able to tell those apart. Reaching
		// this means td's resolution and the mirror in root.go have diverged,
		// which is the failure that mirror is allowed to have.
		health.Status = recall.HealthDegraded
		health.Diagnostics["identity"] = fmt.Sprintf(
			"td opened project %q, but the location %s resolves to %s; "+
				"locators from this source would name a workspace it is not reading",
			info.Project, ws.Location, ws.Root)

	default:
		// The store this instance opened, under the key `recall doctor` reads
		// to find two sources reading one store. See docs/adapter-protocol.md.
		// The resolved root is the honest value because it is what
		// distinguishes two databases sharing a directory name, which td's
		// project name alone cannot — and it is published only here, once the
		// resolution has been confirmed against td's own answer, so a value
		// nothing verified never reaches the check.
		health.Diagnostics[protocol.DiagStoreIdentity] = ws.Root
	}

	// The listing is what a search reads, so probing it here is what makes
	// health and search agree about what exists — and it is the only way to
	// produce a watermark, since td publishes no revision of its own.
	records, raw, err := a.corpus(ctx, set)
	switch {
	case err != nil:
		// td resolved the workspace and then could not list it. The source is
		// reachable, so this is degraded rather than unavailable, and coverage
		// is unknown because nothing has confirmed what the workspace holds.
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexUnknown
		health.Diagnostics["listing"] = "unavailable"

	default:
		health.SourceWatermark = watermark(raw)
		health.Diagnostics["listed"] = len(records)
		if len(records) >= corpusLimit {
			// The listing hit this adapter's own bound, so the watermark
			// fingerprints part of the workspace rather than all of it.
			health.Status = recall.HealthDegraded
			health.Coverage = recall.IndexPartial
			health.Diagnostics["listing"] = "truncated at " + strconv.Itoa(corpusLimit)
		}
	}
	return health, nil
}

// corpus reads the configured slice of the workspace in one invocation.
//
// It returns the raw payload as well as the records, because the payload is
// the only freshness evidence td offers: there is no revision, no cursor, and
// no reliable ordering to quote, but identical bytes mean the same workspace
// state was read, and any change to any issue changes them.
func (a *Adapter) corpus(ctx context.Context, set settings) ([]issue, []byte, error) {
	args := listArgs(set)
	res, err := a.run(ctx, args...)
	if err != nil {
		return nil, nil, err
	}
	var records []issue
	if err := decodeJSON(res, &records, args...); err != nil {
		return nil, nil, err
	}
	return records, res.Stdout, nil
}

// listArgs builds a `td list` invocation over the instance's configured scope.
//
// `--all` is unconditional: without it td hides closed and deferred work, and
// a source that silently forgot finished work would answer "what did we decide
// about X" with nothing. Narrowing is the configured statuses' job, and those
// are applied by td rather than here so the limit applies after the filter.
func listArgs(set settings) []string {
	args := []string{"list", "--json", "--all", "--limit=" + strconv.Itoa(corpusLimit)}
	return append(args, filterArgs(set)...)
}

// searchArgs builds a `td search` invocation for one probe.
//
// The query text goes last, behind `--`, so a term beginning with "-" cannot
// become a flag. td's own filters are used rather than filtering the results
// here, because td applies them before its limit: post-filtering a full result
// page would silently return fewer than the caller asked for, or nothing at
// all for an instance scoped to open work in a mostly-closed workspace.
func searchArgs(set settings, query string, limit int) []string {
	args := []string{"search", "--json", "--limit=" + strconv.Itoa(limit)}
	args = append(args, filterArgs(set)...)
	return append(args, "--", query)
}

func filterArgs(set settings) []string {
	var args []string
	for _, s := range set.Statuses {
		args = append(args, "--status="+s)
	}
	for _, t := range set.Types {
		args = append(args, "--type="+t)
	}
	for _, l := range set.Labels {
		args = append(args, "--labels="+l)
	}
	return args
}

// watermark fingerprints the exact bytes a listing read.
//
// td publishes no workspace revision, so there is nothing to quote. A hash of
// the payload is still real freshness evidence: identical content means the
// same state was read, and any edit anywhere in the workspace changes it,
// because every issue carries its own updated_at.
func watermark(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:8])
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

// keeps reports whether a record passes the instance's configured scope.
//
// td applies the same filters server-side for both probes this adapter issues,
// so this is not where scoping normally happens. It is what an exact id lookup
// is measured against: `td show` ignores filters entirely, and a record fetched
// that way must still be checked, or an instance scoped to open work would
// answer with a closed issue whenever someone pasted its id.
func (s settings) keeps(i issue) bool {
	if len(s.Statuses) > 0 && !containsFold(s.Statuses, i.Status) {
		return false
	}
	if len(s.Types) > 0 && !containsFold(s.Types, i.Type) {
		return false
	}
	if len(s.Labels) > 0 && !anyFold(s.Labels, i.Labels) {
		return false
	}
	return true
}

func containsFold(haystack []string, needle string) bool {
	return slices.ContainsFunc(haystack, func(s string) bool { return strings.EqualFold(s, needle) })
}

func anyFold(want, got []string) bool {
	return slices.ContainsFunc(got, func(g string) bool { return containsFold(want, g) })
}

// parseSettings decodes and validates the adapter-owned settings block.
//
// Unknown keys are rejected. A misspelled setting that silently did nothing
// would be configuration with no code path behind it, which docs/spec.md calls
// a defect rather than a tolerance.
func parseSettings(raw map[string]any) (settings, error) {
	var set settings
	if len(raw) == 0 {
		return set, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams, "td settings: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams, "td settings: %v", err)
	}
	for _, status := range set.Statuses {
		if !containsFold(knownStatuses, status) {
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"td settings: unknown status %q, want one of %s", status, strings.Join(knownStatuses, ", "))
		}
	}
	for _, kind := range set.Types {
		if !containsFold(knownTypes, kind) {
			return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"td settings: unknown type %q, want one of %s", kind, strings.Join(knownTypes, ", "))
		}
	}
	if len(set.Labels) > 1 {
		// td INTERSECTS repeated --labels; this schema documented "any of".
		// Configuring two labels therefore selected issues carrying both, which
		// is usually none, and the source then answered every query with zero
		// candidates while health still reported its full record_count and
		// coverage complete. A false absence presented as a complete boundary is
		// the invariant-5 failure, arriving through configuration where nothing
		// downstream can see it.
		//
		// Refused rather than silently reinterpreted: a union needs one probe
		// per label, and inventing that here would make one configured filter
		// mean something different from the same filter typed at td.
		return settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: labels %v — td intersects repeated labels, so more than one "+
				"selects issues carrying all of them, which is almost never intended and "+
				"cannot be distinguished from an empty workspace. Configure one label, or "+
				"one source instance per label", set.Labels)
	}
	return set, nil
}

// settingsSchema declares the settings block. `recall doctor` validates a
// configuration against it without starting a query, so every key here is one
// the code above actually reads.
func settingsSchema() map[string]any {
	stringList := func(description string, values []string) map[string]any {
		items := map[string]any{"type": "string"}
		if len(values) > 0 {
			enum := make([]any, len(values))
			for i, v := range values {
				enum[i] = v
			}
			items["enum"] = enum
		}
		return map[string]any{
			"type": "array", "items": items, "description": description,
		}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"binary": map[string]any{
				"type":        "string",
				"description": "Path to the td executable. Resolved on PATH when omitted.",
			},
			"workspace": map[string]any{
				"type":        "string",
				"description": "Asserts the workspace identity carried in every locator. The identity is the base name of the directory td resolves the location to, which is not always the location itself; setting this to anything else is refused, because a locator must name the database its source reads.",
			},
			"statuses": stringList("Restrict this source to issues in these td statuses.", knownStatuses),
			"types":    stringList("Restrict this source to these td issue types.", knownTypes),
			// Singular in effect: td intersects repeated --labels, so the adapter
			// refuses more than one rather than let a config mean "all of" while
			// reading as "any of". One source instance per label is the way to
			// express a union.
			"labels": stringList("Restrict this source to issues carrying this label. Configure at most one; td intersects repeated labels.", nil),
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
				"description": "Cap on how many query terms are probed. Each probe is one td invocation; a short multi-word query costs one more for the whole phrase, and every search costs one for the workspace listing.",
			},
			"replay": map[string]any{
				"type":        "string",
				"description": "Directory of recorded td output, relative to the source's location. When set, no process is spawned and every invocation is answered from the recording, which is what lets a committed evaluation pack exercise this adapter.",
			},
		},
	}
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
