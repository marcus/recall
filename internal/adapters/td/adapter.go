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

	// probeLimit bounds what one term probe returns, and it is deliberately
	// not max_candidates.
	//
	// max_candidates caps the OUTPUT list — how many candidates this source
	// hands to fusion — and using it for the input made the one ranking
	// judgment this adapter adds run on partial data that nothing reported.
	// Term coverage counted how many probes had an issue in their own top
	// max_candidates, not how many query terms match the issue. Measured in
	// ~/code/braid at 50, the probe for `source` returned 50 of 242 actual
	// matches and `registry` 50 of 143, so an issue matching every query term
	// but ranking 60th for one of them scored 4 and lost to a weaker issue
	// scoring 5. Nothing said so: `truncated` describes the final list and
	// `corpus_truncated` the listing.
	//
	// The honest bound is the one the listing already uses. A probe cannot
	// return more issues than the workspace holds, and this adapter already
	// declares it reads at most corpusLimit of them, so a probe that reaches
	// this limit is the same runaway workspace the listing bound exists for.
	// Raising it costs payload and no process: measured on braid, the `source`
	// probe took 0.26s returning 50 records and 0.26s returning 242, because
	// what a td invocation costs is the spawn.
	probeLimit = corpusLimit
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

// verifiedWorkspace returns identity that has been tied to `td info`.
//
// Query planning calls Health before Search, but Search deliberately probes
// again. A .td-root or association can change between the two calls, and the
// reviewer finding this closes was specifically that Search trusted a prior
// degraded health result. Expansion can happen much later in a long-lived
// process, so it has the same requirement.
func (a *Adapter) verifiedWorkspace(ctx context.Context) (workspace, error) {
	set, _, _, configured := a.config()
	if set.Replay != "" {
		// A recording has no database to validate and its command sequence is
		// part of the fixture contract. Its configured workspace is the replay
		// namespace; the recorded td project may intentionally differ.
		return configured, nil
	}
	info, _, err := a.probeWorkspace(ctx)
	if err != nil {
		return workspace{}, err
	}
	ws, err := bindWorkspace(configured, info)
	if err != nil {
		return workspace{}, err
	}
	return ws, nil
}

// verifyWorkspaceUnchanged closes the check/use interval around evidence
// reads. td is a one-shot CLI, so identity and evidence cannot be returned by
// one process on current td; a mandatory post-read probe is the fail-closed
// alternative. The caller discards everything it read when this fails.
func (a *Adapter) verifyWorkspaceUnchanged(ctx context.Context, before workspace) error {
	after, err := a.verifiedWorkspace(ctx)
	if err != nil {
		return err
	}
	if before.Root != after.Root || before.Name != after.Name {
		return fmt.Errorf("%w: td workspace changed while evidence was being read; evidence discarded",
			protocol.ErrSourceUnavailable)
	}
	return nil
}

// probeWorkspace asks td which database it opened. It is kept separate from
// Health so a direct Search or Expand cannot bypass the identity check.
func (a *Adapter) probeWorkspace(ctx context.Context) (workspaceInfo, Result, error) {
	args := []string{"info", "--json"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return workspaceInfo{}, res, err
	}
	var info workspaceInfo
	if err := decodeJSON(res, &info, args...); err != nil {
		return workspaceInfo{}, res, err
	}
	return info, res, nil
}

// bindWorkspace turns td's report into the only workspace identity operations
// may use. Configuration and the filesystem mirror are hints until this
// succeeds.
func bindWorkspace(configured workspace, info workspaceInfo) (workspace, error) {
	project := strings.TrimSpace(info.Project)
	if err := checkWorkspaceName(project); err != nil {
		return workspace{}, fmt.Errorf("%w: td info reported an invalid project: %w",
			protocol.ErrSourceUnavailable, err)
	}

	root := configured.Root
	authoritativeRoot := false
	if strings.TrimSpace(info.Root) != "" {
		if !filepath.IsAbs(info.Root) {
			return workspace{}, fmt.Errorf("%w: td info reported a non-absolute workspace root",
				protocol.ErrSourceUnavailable)
		}
		root = canonicalPath(info.Root)
		authoritativeRoot = true
	}
	if filepath.IsAbs(info.Database) {
		database := canonicalPath(info.Database)
		if filepath.Base(database) != "issues.db" || filepath.Base(filepath.Dir(database)) != todosDir {
			return workspace{}, fmt.Errorf("%w: td info database %q does not identify a .todos/issues.db store",
				protocol.ErrSourceUnavailable, filepath.Base(info.Database))
		}
		databaseRoot := filepath.Dir(filepath.Dir(database))
		if authoritativeRoot && canonicalPath(root) != canonicalPath(databaseRoot) {
			return workspace{}, fmt.Errorf("%w: td info root and database identify different stores",
				protocol.ErrSourceUnavailable)
		}
		root = databaseRoot
		authoritativeRoot = true
	}
	root = canonicalPath(root)

	// Current td reports a relative ".todos/issues.db". Refuse paths that
	// could escape the verified root; they cannot name the store at that root.
	if database := strings.TrimSpace(info.Database); database != "" && !filepath.IsAbs(database) {
		clean := filepath.Clean(database)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return workspace{}, fmt.Errorf("%w: td info database escapes its workspace root",
				protocol.ErrSourceUnavailable)
		}
		if clean != filepath.Join(todosDir, "issues.db") {
			return workspace{}, fmt.Errorf("%w: td info database does not identify .todos/issues.db",
				protocol.ErrSourceUnavailable)
		}
	}

	rootProject := filepath.Base(root)
	if !strings.EqualFold(project, rootProject) {
		kind := "resolved"
		if authoritativeRoot {
			kind = "reported"
		}
		return workspace{}, fmt.Errorf("%w: td opened project %q, but its %s root names workspace %q; "+
			"locators from this source would name a workspace it is not reading",
			protocol.ErrSourceUnavailable, project, kind, rootProject)
	}
	if configured.Asserted != "" && !strings.EqualFold(configured.Asserted, project) {
		return workspace{}, fmt.Errorf("%w: td settings assert workspace %q, but td opened project %q; "+
			"a configured name cannot rename another workspace's database",
			protocol.ErrSourceUnavailable, configured.Asserted, project)
	}

	configured.Name = rootProject
	configured.Root = root
	configured.StoreIdentity = storeIdentity(root)
	return configured, nil
}

// Health probes the workspace with one `td info`, and nothing else.
//
// The honest health question for this source is not "did a command return" but
// "does this workspace resolve to a database td can read". `td info` answers
// exactly that, and it answers it with the workspace's own name and issue
// counts, so a location pointing at the wrong repository is visible in
// diagnostics rather than silently returning another project's work.
//
// One invocation is the whole probe on purpose. Health is called once per
// source per query before anything is searched, so whatever it costs is charged
// to every question asked of this machine. See the freshness note further down
// for what reading the workspace listing here used to buy and where that
// evidence lives now.
//
// A missing, unreadable, or uninitialized workspace is unavailable. It is
// never a successful search with no matches: td exits non-zero and says
// `database not found`, and reporting that as an empty corpus would let fusion
// downstream treat "the workspace is gone" as "there is no such issue".
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, _, _, configured := a.config()
	checked := a.now()

	info, res, err := a.probeWorkspace(ctx)
	if err != nil {
		return adapter.Unhealthy(err), nil
	}
	ws := configured
	if set.Replay == "" {
		ws, err = bindWorkspace(configured, info)
	}
	if err != nil {
		// Health reports source failures in its status; a Go error here would
		// make callers discard the structured diagnostics and coverage.
		//nolint:nilerr
		return recall.Health{
			Status:    recall.HealthUnavailable,
			CheckedAt: checked,
			Coverage:  recall.IndexUnknown,
			Diagnostics: map[string]any{
				"workspace":      configured.Name,
				"location_label": locationLabel(configured.Location),
				"td_project":     info.Project,
				"identity":       err.Error(),
			},
		}, nil
	}
	health := recall.Health{
		Status:      recall.HealthHealthy,
		CheckedAt:   checked,
		LastSuccess: &checked,
		RecordCount: info.Issues.Total,
		Coverage:    recall.IndexComplete,
		Diagnostics: map[string]any{
			"workspace":      ws.Name,
			"location_label": locationLabel(ws.Location),
			"td_project":     info.Project,
			"open":           info.Issues.Open,
			"in_progress":    info.Issues.InProgress,
			"blocked":        info.Issues.Blocked,
			"in_review":      info.Issues.InReview,
			"closed":         info.Issues.Closed,
			"cli_wall_ms":    res.Elapsed.Milliseconds(),
		},
	}
	if ws.Asserted != "" {
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

	default:
		// The store this instance opened, under the key `recall doctor` reads
		// to find two sources reading one store. See docs/adapter-protocol.md.
		// The resolved root is the honest input because it is what
		// distinguishes two databases sharing a directory name, which td's
		// project name alone cannot. Only its deterministic opaque digest is
		// published, after resolution has been confirmed, so neither an
		// unverified value nor an absolute path reaches the check.
		health.Diagnostics[protocol.DiagStoreIdentity] = ws.StoreIdentity
	}

	// `td info` is the whole of this probe, and the freshness evidence health
	// can report is bounded by that.
	//
	// It used to read the workspace listing here as well, because td publishes
	// no revision of its own and a hash of the listing is the only fingerprint
	// this source can produce. That put 1.6 MB of JSON — the largest workspace
	// in this deployment — on the path of every query, twice, to report a
	// watermark nothing on that path reads. Cheaper substitutes were looked for
	// and refused: `td info` carries a session id that changes between
	// invocations, and td's `--sort` silently ignores a field it does not know,
	// so a watermark built on "the most recently updated issue" would have been
	// a value that stops changing when td changes its sort keys. A watermark
	// that quietly stops moving is worse than none.
	//
	// The listing watermark is not lost where it decides anything. Search reads
	// the listing because it needs the structured fallback and the free id
	// lookups anyway, and stamps the fingerprint on the response and on every
	// candidate as its source_revision, which is where freshness reaches an
	// answer. What a health-only probe can no longer produce is a watermark of
	// its own, and this says so rather than leaving the field silently empty.
	health.Diagnostics["watermark"] = "not read here: td publishes no revision, and its only fingerprint is the workspace listing, which search reads and health does not"

	if scope := set.scopeBound(info); scope >= corpusLimit {
		// A listing for this instance would stop at this adapter's bound before
		// it reached the end of the scope, so what search confirms present is
		// part of the workspace and not all of it. Read off td's own counts
		// rather than by listing: the counts are an UPPER bound on what a
		// listing would return, since a type or label filter only narrows them
		// further, so this declares incomplete coverage early rather than late.
		// Early is the safe direction for a claim about a boundary.
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexPartial
		health.Diagnostics["listing"] = fmt.Sprintf(
			"would truncate: %d issues in this source's scope, and a listing reads at most %d",
			scope, corpusLimit)
	}
	return health, nil
}

// scopeBound is an upper bound on how many issues a listing for this instance
// would return, taken from the counts `td info` reports.
//
// Statuses narrow it exactly, because td counts issues per status. Types and
// labels are not represented: they only narrow the result further, and the one
// claim made on this number is "a listing might not fit", which an over-estimate
// makes early rather than wrongly. Guessing a narrower number would be the
// opposite trade — a source that lists half its scope while reporting complete
// coverage, which is the false-boundary failure this adapter exists to avoid.
func (s settings) scopeBound(info workspaceInfo) int64 {
	if len(s.Statuses) == 0 {
		return info.Issues.Total
	}
	var bound int64
	for _, status := range s.Statuses {
		switch strings.ToLower(status) {
		case "open":
			bound += info.Issues.Open
		case "in_progress":
			bound += info.Issues.InProgress
		case "blocked":
			bound += info.Issues.Blocked
		case "in_review":
			bound += info.Issues.InReview
		case "closed":
			bound += info.Issues.Closed
		}
	}
	return bound
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
				"description": "Cap on candidates returned by one search. It bounds the output list only: a term probe reads the workspace under this adapter's own bound, so lowering this cannot change which issue outranks which.",
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
