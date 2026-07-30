package qmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Adapter identity, reported in the manifest.
const (
	AdapterID   = "recall-qmd/1"
	DisplayName = "QMD"
)

// Options are the seams a test uses. Live configuration goes through settings.
type Options struct {
	// Runner replaces both the executable and the replay pack. A nil value
	// builds one from the settings block, which is how a configured source and
	// a committed evaluation pack reach the same code.
	Runner Runner

	// Clock replaces the wall clock. A nil value prefers a replay pack's stated
	// clock and falls back to time.Now.
	Clock func() time.Time
}

// Adapter is a qmd collection as a Recall source.
//
// It owns no index. qmd owns the SQLite index, this process shells out to the
// qmd CLI to search it, and evidence is read from the corpus files rather than
// through qmd. What the adapter owns is the honesty layer: identity, coverage,
// relevance, locators, and the classification of every way qmd can fail.
type Adapter struct {
	opts Options

	mu          sync.RWMutex
	ready       bool
	closed      bool
	sourceID    string
	location    string
	settings    Settings
	runner      Runner
	lastSuccess *time.Time
	lastRefresh string
}

func New(opts Options) *Adapter { return &Adapter{opts: opts} }

var _ adapter.Adapter = (*Adapter)(nil)

// ErrNotInitialized reports use before a handshake.
var ErrNotInitialized = protocol.Errorf(protocol.CodeSourceUnavailable,
	"qmd adapter has not completed a handshake")

// Initialize negotiates a version, validates the settings block, and resolves
// the corpus location.
//
// It deliberately spawns nothing. The handshake competes with the core's
// 10-second timeout, and every qmd probe worth making — the collection check,
// the index counts, the model identities — is cheap enough to make per
// operation but pointless to make here: a missing qmd binary must be reportable
// as an unavailable source with a reason, and a handshake that failed on it
// would leave the source unable to report anything at all. Model warm-up and
// indexing belong to recall/refresh for the same reason, with the added one that
// a first refresh downloads about 2GB.
func (a *Adapter) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	set, err := parseSettings(cfg.Settings, cfg.BaseDir)
	if err != nil {
		return recall.Manifest{}, err
	}

	location := strings.TrimSpace(cfg.Location)
	if location == "" {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeInvalidParams,
			"qmd: this source names no location; a qmd source is one indexed corpus directory")
	}
	if !filepath.IsAbs(location) {
		base := cfg.BaseDir
		if base == "" {
			if base, err = os.Getwd(); err != nil {
				return recall.Manifest{}, err
			}
		}
		location = filepath.Join(base, location)
	}
	location = filepath.Clean(location)
	if info, err := os.Stat(location); err != nil || !info.IsDir() {
		// Expansion reads these files directly, so a location that is not a
		// readable directory cannot serve evidence for anything a search
		// returns. Refusing at the handshake names the misconfiguration once
		// instead of once per expansion.
		return recall.Manifest{}, protocol.Errorf(protocol.CodeSourceUnavailable,
			"qmd: corpus location %q is not a readable directory", labelOf(location))
	}
	set.Binary = effectiveBinary(set.Binary, location)

	runner := a.opts.Runner
	if runner == nil {
		if set.Replay != "" {
			if runner, err = NewReplayRunner(set.Replay); err != nil {
				return recall.Manifest{}, err
			}
		} else {
			runner = ExecRunner{Binary: set.Binary, Timeout: set.Timeout, Dir: location}
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.ready = true
	a.sourceID = cfg.SourceID
	a.location = location
	a.settings = set
	a.runner = runner
	a.lastSuccess = nil
	a.lastRefresh = ""

	manifest := recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		// Declared per mode, because a mode this source is not running is a
		// capability it does not have. A bm25 instance declaring `semantic`
		// would be eligible for requests it would answer as a keyword search,
		// which is exactly the unattributable blend the mode setting exists to
		// take apart. `exact` is never declared: qmd matches text, and this
		// adapter has no identifier lookup to promote.
		QueryModes:     queryModes(set.Mode),
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		// none, not filter. qmd exposes no time filter and its results carry no
		// event time; the only date available is a file mtime, which is a
		// property of this checkout rather than of the record. Declaring filter
		// and answering from an mtime would answer a historical question from
		// current state.
		AsOfSupport:    recall.AsOfNone,
		RelevanceBasis: relevanceBasis(set.Mode),
		Capabilities:   []recall.Capability{recall.CapSearch, recall.CapExpand, recall.CapCheckpoint},
		// One at a time. Every qmd invocation is a fresh process that memory-maps
		// its GGUF models, so two concurrent searches cost two model loads on a
		// machine that has already decided one is expensive. Declaring the limit
		// is the honest alternative to pretending the source is cheap to fan out.
		MaxConcurrency:  1,
		FreshnessPolicy: freshnessPolicy(set.Mode),
		Sensitivity:     recall.SensitivityInternal,
		SettingsSchema:  settingsSchema(),
	}
	if set.Replay == "" && a.opts.Runner == nil {
		manifest.ExecutableRequirements = []recall.ExecutableRequirement{{
			Name:    "qmd",
			Command: set.Binary,
			Purpose: "search and refresh the configured qmd collection",
		}}
	}
	return manifest, nil
}

// effectiveBinary resolves slash-containing relative commands against the
// corpus working directory used by ExecRunner. A bare name intentionally stays
// bare for PATH lookup. Returning this same value in the manifest makes doctor
// preflight the exact command execution will use.
func effectiveBinary(binary, location string) string {
	if filepath.IsAbs(binary) {
		return filepath.Clean(binary)
	}
	if strings.ContainsRune(binary, filepath.Separator) {
		return filepath.Clean(filepath.Join(location, binary))
	}
	return binary
}

func queryModes(mode Mode) []recall.QueryMode {
	switch mode {
	case ModeBM25:
		return []recall.QueryMode{recall.QueryLexical}
	case ModeVector:
		return []recall.QueryMode{recall.QuerySemantic}
	default:
		return []recall.QueryMode{recall.QueryLexical, recall.QuerySemantic}
	}
}

func freshnessPolicy(mode Mode) string {
	policy := "indexed: qmd owns the index and recall/refresh rebuilds it. " +
		"A collection whose indexed documents are not all embedded is reported degraded and keeps answering; " +
		"watermarks are counts, so an edit that changes neither the document count nor the vector count is invisible until a refresh"
	if mode.Embeds() {
		return policy + "; an index holding no vectors makes this mode unavailable rather than empty"
	}
	return policy + "; this mode reads the full-text index only and needs no embeddings"
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.ready = false
	a.runner = nil
	return nil
}

func (a *Adapter) session() (Settings, string, string, Runner, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.closed:
		return Settings{}, "", "", nil, adapter.ErrClosed
	case !a.ready || a.runner == nil:
		return Settings{}, "", "", nil, ErrNotInitialized
	}
	return a.settings, a.sourceID, a.location, a.runner, nil
}

// corpusRoot is the directory evidence is read from.
//
// A replay pack ships its own corpus, because expansion does not go through qmd
// and a pack with recorded search output but no files could not exercise it.
func (a *Adapter) corpusRoot() (string, error) {
	_, _, location, runner, err := a.session()
	if err != nil {
		return "", err
	}
	if root, ok := runner.Root(); ok {
		return root, nil
	}
	return location, nil
}

func (a *Adapter) now() time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	a.mu.RLock()
	runner := a.runner
	a.mu.RUnlock()
	if runner != nil {
		if stated, ok := runner.Now(); ok {
			return stated.UTC()
		}
	}
	return time.Now().UTC()
}

func (a *Adapter) noteSuccess(at time.Time) {
	at = at.UTC()
	a.mu.Lock()
	a.lastSuccess = &at
	a.mu.Unlock()
}

func (a *Adapter) lastSuccessful() *time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.lastSuccess == nil {
		return nil
	}
	got := *a.lastSuccess
	return &got
}

// run spawns one read-only qmd invocation.
func (a *Adapter) run(ctx context.Context, args ...string) (Result, error) {
	return a.spawn(ctx, false, args...)
}

// runMaintenance spawns one index-mutating qmd invocation. Only [Adapter.Refresh]
// calls it, which is what keeps a search from rebuilding an index as a side
// effect of a query.
func (a *Adapter) runMaintenance(ctx context.Context, args ...string) (Result, error) {
	return a.spawn(ctx, true, args...)
}

func (a *Adapter) spawn(ctx context.Context, maintenance bool, args ...string) (Result, error) {
	if err := checkAllowed(args, maintenance); err != nil {
		return Result{}, err
	}
	_, _, _, runner, err := a.session()
	if err != nil {
		return Result{}, err
	}
	return runner.Run(ctx, args...)
}

// Health probes qmd, the collection, and the index counts.
//
// The honest health question for this source is not "did a command return" but
// "does the configured collection still index the corpus this source was
// configured for, and does the index hold enough to answer in this mode". Both
// halves are checked here, and the same coverage decision is what Search
// reports, so a search that answers partial cannot sit beside a probe claiming
// complete.
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, _, location, runner, err := a.session()
	if err != nil {
		if errors.Is(err, adapter.ErrClosed) {
			return recall.Health{}, err
		}
		return a.unhealthy(err), nil
	}

	health := recall.Health{
		Status:      recall.HealthUnavailable,
		CheckedAt:   a.now(),
		LastSuccess: a.lastSuccessful(),
		Coverage:    recall.IndexUnknown,
		Diagnostics: map[string]any{
			"transport":  runner.Kind(),
			"mode":       string(set.Mode),
			"collection": set.Collection,
		},
	}
	a.mu.RLock()
	lastRefresh := a.lastRefresh
	a.mu.RUnlock()
	if lastRefresh != "" {
		health.Diagnostics["last_refresh_error"] = lastRefresh
	}

	report, collection, err := a.probeIndex(ctx, location)
	if err != nil {
		if ctx.Err() != nil {
			return recall.Health{}, err
		}
		_, reason := adapter.Classify(err)
		health.Diagnostics["reason"] = reason
		health.Diagnostics["detail"] = safeErr(err)
		if errors.Is(err, protocol.ErrSourceDenied) {
			health.Status = recall.HealthDenied
		}
		return health, nil
	}

	version := a.version(ctx)
	coverage, why := coverageOf(report, set)
	health.SourceWatermark = sourceWatermark(report, set)
	health.IndexWatermark = indexWatermark(report)
	health.IndexGeneration = indexGeneration(version, report, set)
	health.IndexModel = indexModel(report, set)
	health.IndexConfig = indexConfig(version, report, set)
	health.RecordCount = int64(report.Collection.Files)
	health.IndexedCount = int64(report.Collection.Files)
	health.Diagnostics["index_documents"] = report.Documents
	health.Diagnostics["index_vectors"] = report.Vectors
	health.Diagnostics["qmd_version"] = version
	health.Diagnostics["included_by_default"] = collection.Included
	if why != "" {
		health.Diagnostics["coverage_reason"] = why
	}
	// Set only for a live store. A replayed instance opened nothing, and a
	// value derived from a fixture directory would make two packs in one
	// profile look like two sources over one index.
	if runner.Kind() == "live" {
		if id := storeIdentity(report.IndexPath, set.Collection); id != "" {
			health.Diagnostics[protocol.DiagStoreIdentity] = id
		}
	}

	switch coverage {
	case coverageComplete:
		health.Status, health.Coverage = recall.HealthHealthy, recall.IndexComplete
		a.noteSuccess(health.CheckedAt)
		health.LastSuccess = a.lastSuccessful()
	case coveragePartial:
		health.Status, health.Coverage = recall.HealthDegraded, recall.IndexPartial
	case coverageNone:
		health.Status, health.Coverage = recall.HealthUnavailable, recall.IndexPartial
	default:
		health.Status, health.Coverage = recall.HealthDegraded, recall.IndexUnknown
	}
	return health, nil
}

func (a *Adapter) unhealthy(err error) recall.Health {
	status := recall.HealthUnavailable
	if errors.Is(err, protocol.ErrSourceDenied) {
		status = recall.HealthDenied
	}
	_, reason := adapter.Classify(err)
	return recall.Health{
		Status:      status,
		CheckedAt:   a.now(),
		Coverage:    recall.IndexUnknown,
		Diagnostics: map[string]any{"reason": reason, "detail": safeErr(err)},
	}
}

// version asks qmd what it is. A failure is not fatal: index_config reports the
// version as unknown, which is worse than knowing it and better than pretending.
func (a *Adapter) version(ctx context.Context) string {
	args := []string{"--version"}
	res, err := a.run(ctx, args...)
	if err != nil {
		return ""
	}
	text, err := decodeText(res, args...)
	if err != nil {
		return ""
	}
	return parseVersion(text)
}

// Refresh reindexes the collection, embeds it, and warms the models.
//
// This is the only place this adapter mutates anything, and it is the only
// in-contract place to build an index outside the handshake. The warm query at
// the end is not cosmetic: qmd's first invocation after an install or a model
// eviction writes a spinner and download progress to STDOUT, ahead of the JSON
// document `--json` promises, so a cold model load reaching a search would be
// read as a broken contract. Paying for it here means a search never does.
//
// A build that fails is reported through the returned health rather than as an
// error. A JSON-RPC frame carries a result or an error and never both, so an
// error return would discard the health of the index that is still there and
// still answering. The error return is reserved for a refresh that could not be
// attempted at all.
func (a *Adapter) Refresh(ctx context.Context, p protocol.RefreshParams) (recall.Health, error) {
	set, _, location, _, err := a.session()
	if err != nil {
		if errors.Is(err, adapter.ErrClosed) {
			return recall.Health{}, err
		}
		return a.unhealthy(err), nil
	}
	if !p.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, p.Deadline)
		defer cancel()
	} else if set.RefreshTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, set.RefreshTimeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return recall.Health{}, err
	}

	// Capture the boundary before maintenance. Count watermarks are weak, but
	// their direction is still useful: a corpus changing during a pass can
	// leave the post-refresh health partial even though every comparable count
	// moved forward. That is successful maintenance progress, not healthy query
	// coverage; the two are reported separately.
	before, _, beforeErr := a.probeIndex(ctx, location)

	failure := ""
	for _, step := range a.refreshSteps(set) {
		res, err := a.runMaintenance(ctx, step.args...)
		if err != nil {
			// Cancellation and an expired deadline are not index health: the
			// caller abandoned this maintenance operation, and the host needs
			// the error to preserve cancellation semantics across every surface.
			if ctx.Err() != nil {
				return recall.Health{}, err
			}
			failure = step.name + ": " + safeErr(err)
			break
		}
		if _, err := step.check(res); err != nil {
			failure = step.name + ": " + safeErr(err)
			break
		}
	}

	a.mu.Lock()
	a.lastRefresh = failure
	a.mu.Unlock()

	// Probed after the attempt either way, so the report describes the index as
	// it now is. WithoutCancel because the work is already done: a probe that
	// failed because the caller's context expired in between would discard the
	// only account of what the refresh accomplished.
	health, err := a.Health(context.WithoutCancel(ctx))
	if err != nil {
		return recall.Health{}, err
	}
	if failure != "" && health.Status == recall.HealthHealthy {
		// The index is usable and this refresh did not move it forward. Saying
		// healthy would hide a build that is not progressing.
		health.Status = recall.HealthDegraded
	}
	if failure == "" && beforeErr == nil {
		health.CheckpointProgress = checkpointProgress(before, health)
		if health.CheckpointProgress == recall.CheckpointRegressed {
			health.Status = recall.HealthDegraded
			if health.Diagnostics == nil {
				health.Diagnostics = map[string]any{}
			}
			health.Diagnostics["checkpoint_regression"] =
				"one or more comparable qmd counts moved backward during refresh"
		}
	}
	return health, nil
}

// checkpointProgress compares only values qmd reported on both sides. Missing
// parse data is incomparable and returns the zero value; no caller is allowed
// to turn unknown counters into progress.
func checkpointProgress(before indexReport, after recall.Health) recall.CheckpointProgress {
	if !before.HasCounts || !before.Collection.HasFiles ||
		after.IndexWatermark == "" ||
		strings.Contains(after.SourceWatermark, "files=?") {
		return ""
	}
	docs, docsOK := after.Diagnostics["index_documents"].(int)
	vectors, vectorsOK := after.Diagnostics["index_vectors"].(int)
	if !docsOK || !vectorsOK {
		return ""
	}
	files := int(after.RecordCount)
	regressed := files < before.Collection.Files ||
		docs < before.Documents ||
		vectors < before.Vectors
	if regressed {
		return recall.CheckpointRegressed
	}
	if files > before.Collection.Files ||
		docs > before.Documents ||
		vectors > before.Vectors {
		return recall.CheckpointAdvanced
	}
	return recall.CheckpointUnchanged
}

type refreshStep struct {
	name  string
	args  []string
	check func(Result) (any, error)
}

// refreshSteps is the maintenance sequence, in the order it has to run: reindex,
// embed, then one throwaway query that forces the models this mode needs to
// load. A bm25 instance skips the last two: it consults no model, and paying
// for a 2GB download to serve a full-text search would be a cost nothing in
// that configuration ever recovers.
func (a *Adapter) refreshSteps(set Settings) []refreshStep {
	text := func(res Result) (any, error) { return decodeText(res, "update") }
	steps := []refreshStep{
		{name: "update", args: []string{"update"}, check: text},
	}
	if !set.Mode.Embeds() {
		return steps
	}
	steps = append(steps,
		refreshStep{name: "embed", args: []string{"embed", "-c", set.Collection}, check: text},
		refreshStep{
			name: "warm",
			args: searchArgs(set.Mode, set.Collection, 1, warmQuery),
			check: func(res Result) (any, error) {
				hits, err := decodeResults(res, "warm")
				return hits, err
			},
		},
	)
	return steps
}

// warmQuery is the throwaway query that loads the models. Its text is
// deliberately ordinary prose: an empty or punctuation-only query is refused by
// qmd's own grammar, and a query naming the corpus would make the warm-up
// depend on what is in it.
const warmQuery = "recall adapter model warm up"

// safeErr renders an error for a diagnostic a person reads: one line, scrubbed,
// bounded, and with no absolute path in it.
func safeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := sanitizeLine(err.Error())
	if len(msg) > safeDetailLimit {
		return msg[:cutRunes(msg, safeDetailLimit)] + "…"
	}
	return msg
}

// searchArgs builds the argv for one mode. It is the single place a mode becomes
// a command line, so the mapping the documentation states is the mapping that
// runs.
func searchArgs(mode Mode, collection string, limit int, query string) []string {
	count := fmt.Sprint(limit)
	switch mode {
	case ModeBM25:
		return []string{"search", "--json", "-n", count, "-c", collection, "--", query}
	case ModeVector:
		return []string{"vsearch", "--json", "-n", count, "-c", collection, "--", query}
	case ModeHybrid:
		return []string{"query", "--json", "--explain", "--no-rerank", "-n", count, "-c", collection, "--", query}
	default:
		return []string{"query", "--json", "--explain", "-n", count, "-c", collection, "--", query}
	}
}
