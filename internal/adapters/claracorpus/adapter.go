package claracorpus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	AdapterID = "recall-clara-corpus/1"

	// DisplayName is the adapter's name. The instance's name is its configured
	// source_id and arrives at the handshake.
	DisplayName = "Clara Corpus"

	// RecordMemory is the record type a memory candidate carries.
	//
	// docs/adapter-protocol.md declares the record type set open — "person |
	// task | document | message | event | ..." — and the wire schema honors
	// that. A distilled fact about the owner is none of the named five: it is
	// not a document, and calling it one would flatten a weighted, decaying,
	// subject-keyed record into prose the moment anything filtered on type.
	RecordMemory recall.RecordType = "memory"

	// DefaultMaxCandidates caps one search's candidate list so a long-lived
	// corpus cannot flood the fusion pool. The core's per-source limit narrows
	// it further.
	DefaultMaxCandidates = 50

	// IndexConfig identifies the retrieval configuration a generation was built
	// under. It changes whenever tokenization or scoring changes, so an
	// evaluation comparing two generations cannot mistake a scoring change for
	// the change under test.
	IndexConfig = "clara-corpus/1 tokenizer=ident-runs scoring=term-coverage×clara-effective-weight"

	// checkpointFile is the only file this adapter writes, and it is written
	// only inside the handshake's workdir.
	checkpointFile = "cursor.json"
)

// storeKind selects which of Clara's stores one instance serves. It is required
// configuration: an adapter that guessed would answer a question about memory
// out of the signal stream the first time a corpus held only one of them.
type storeKind string

const (
	// StoreSignals serves signals.jsonl and signals-archive.jsonl, with
	// observations.jsonl projected onto them.
	StoreSignals storeKind = "signals"

	// StoreMemory serves memory.jsonl and memory-archive.jsonl.
	StoreMemory storeKind = "memory"
)

// storeKinds is the accepted set, in the order the settings schema lists it.
var storeKinds = []storeKind{StoreSignals, StoreMemory}

// Clara's store file names. They are the corpus contract, not configuration:
// Clara's own Config resolves data_dir and then appends exactly these.
const (
	fileSignals        = "signals.jsonl"
	fileSignalsArchive = "signals-archive.jsonl"
	fileObservations   = "observations.jsonl"
	fileMemory         = "memory.jsonl"
	fileMemoryArchive  = "memory-archive.jsonl"
)

// claraMarkers are the files whose presence proves a directory is a Clara data
// directory. A location that holds none of them is refused at the handshake
// rather than answered as an empty store: a typo in a path would otherwise
// become a source that reports complete coverage over nothing, which is the
// false absence invariant 5 exists to prevent.
var claraMarkers = []string{
	fileSignals, fileSignalsArchive, fileObservations,
	fileMemory, fileMemoryArchive, "run-manifests.jsonl", "state.json",
}

// Settings is the adapter-owned settings block, declared by
// [recall.Manifest.SettingsSchema] and validated here on every handshake.
type Settings struct {
	// Store selects which of Clara's stores this instance serves. Required.
	Store string `json:"store"`

	// Upstream maps a Clara source name — the `source` field on a signal, and
	// the prefix of the `ref` it carries — to the source_id of the Recall
	// source that owns those records. It is what turns a projection into a
	// derived_from edge; a source absent here yields no edge.
	Upstream map[string]string `json:"upstream"`

	// Timezone is the IANA zone Clara computes its civil "today" in. Decay ages
	// a record in whole days from a civil date, so a zone that disagrees with
	// Clara's own config moves every boundary by up to a day. Empty means UTC,
	// and health says which zone was used.
	Timezone string `json:"timezone"`

	MaxCandidates int `json:"max_candidates"`

	// Today fixes the civil date memory records are aged from.
	//
	// Decay is the one thing this adapter computes whose answer changes every
	// morning, which would make a recorded transcript about decay drift a day
	// at a time until it failed, and a benchmark over a memory corpus
	// irreproducible. Pinning the date is what lets both hold the arithmetic
	// still. It is refused on a signals instance, where nothing ages anything.
	// Leave it unset in real configuration.
	Today string `json:"debug_today"`

	// StallMS delays every search before it scans. It exists so the
	// cancellation conformance case can be recorded deterministically against a
	// real process, and it is the shortest demonstration of the only thing an
	// adapter owes a cancel: notice the context, return, do not answer. Leave
	// it unset in real configuration.
	StallMS int `json:"debug_stall_ms"`
}

// Options are construction-time seams. Instance policy arrives at the
// handshake, because that is where the spec puts it.
type Options struct {
	// Clock supplies build and probe timestamps, and the civil date decay is
	// measured against. Nil means [time.Now].
	Clock func() time.Time
}

// Adapter is one Clara store as a Recall source.
//
// The zero value is not usable; build one with [New]. It holds a published
// index generation and the file stamps that produced it.
type Adapter struct {
	opts Options

	mu       sync.RWMutex
	ready    bool
	closed   bool
	sourceID string
	workdir  string
	dir      string
	identity string
	store    storeKind
	loc      *time.Location
	set      Settings
	floor    recall.Sensitivity
	gen      int64
	prior    checkpoint
	snap     *snapshot
	cpFailed bool

	// buildMu serializes scans. Two concurrent searches must not both reparse
	// the store; one builds, the other reads the generation it publishes.
	buildMu sync.Mutex
}

// New returns an uninitialized adapter.
func New(opts Options) *Adapter { return &Adapter{opts: opts} }

// ErrNotInitialized reports use before a successful handshake. It carries the
// source_unavailable code, so errors.Is against the sentinel still matches,
// while keeping a message the wire can show.
var ErrNotInitialized = protocol.Errorf(protocol.CodeSourceUnavailable,
	"clara-corpus adapter has not completed a handshake")

// Initialize negotiates the protocol version, validates the settings block,
// resolves the Clara data directory, and adopts the workdir's checkpoint. It
// parses no records: building an index inside the handshake competes with the
// handshake timeout, which is why recall/refresh exists.
func (a *Adapter) Initialize(ctx context.Context, cfg adapter.Config) (recall.Manifest, error) {
	if err := ctx.Err(); err != nil {
		return recall.Manifest{}, err
	}
	version := min(protocol.MaxVersion, cfg.ProtocolVersionMax)
	if err := protocol.CheckNegotiated(cfg, version); err != nil {
		return recall.Manifest{}, err
	}
	if version < protocol.MinVersion {
		// The requested range lies entirely below what this build speaks.
		// Failing is the contract: degrading to a version neither end
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
	loc, err := zone(set.Timezone)
	if err != nil {
		return recall.Manifest{}, err
	}
	dir, err := resolveDataDir(cfg.Location)
	if err != nil {
		return recall.Manifest{}, err
	}
	if strings.TrimSpace(cfg.Workdir) == "" {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus: handshake supplied no workdir, and this adapter has nowhere else it may write")
	}
	if err := os.MkdirAll(cfg.Workdir, 0o700); err != nil {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeSourceUnavailable,
			"clara-corpus: workdir is not writable: %v", err)
	}

	store := storeKind(set.Store)
	prior := loadCheckpoint(cfg.Workdir)
	if err := ctx.Err(); err != nil {
		return recall.Manifest{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.sourceID = cfg.SourceID
	a.workdir = cfg.Workdir
	a.dir = dir
	// The store this instance OPENED, not the one configuration named. See the
	// package doc and protocol.DiagStoreIdentity.
	a.identity = dir + "#" + string(store)
	a.store = store
	a.loc = loc
	a.set = set
	a.floor = store.floor()
	// Generations continue from the last published one, so an id never names
	// two different builds of this workdir.
	a.gen = prior.Generation
	a.prior = prior
	a.snap = nil
	a.ready = true

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,
		RecordTypes:     store.recordTypes(),
		QueryModes: []recall.QueryMode{
			recall.QueryExact, recall.QueryLexical, recall.QueryStructured, recall.QueryTemporal,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},

		// Clara rewrites these records in place on every ingest, reinforcement,
		// and consolidation, and publishes no revision history. Selecting by an
		// event time would return each record as it is now with a boundary
		// attached, which is answering a historical question from current
		// state. See the package doc.
		AsOfSupport: recall.AsOfNone,

		Capabilities: []recall.Capability{
			recall.CapSearch, recall.CapExpand, recall.CapCheckpoint,
		},
		FreshnessPolicy: store.freshnessPolicy(),
		Sensitivity:     a.floor,
		SettingsSchema:  settingsSchema(),
	}, nil
}

// recordTypes is what this store can return.
//
// Signals keep the shape of what they project — a todo stays a task, an email
// stays a message — because flattening them into one anonymous type would
// discard the routing signal a record_type filter exists to use, and would put
// a signal and the task it projects into different types while they share one
// lineage root.
func (k storeKind) recordTypes() []recall.RecordType {
	if k == StoreMemory {
		return []recall.RecordType{RecordMemory}
	}
	return []recall.RecordType{recall.RecordTask, recall.RecordMessage, recall.RecordEvent}
}

// floor is the source's declared classification floor. See the package doc for
// why the two stores differ.
func (k storeKind) floor() recall.Sensitivity {
	if k == StoreMemory {
		return recall.SensitivityConfidential
	}
	return recall.SensitivityInternal
}

func (k storeKind) freshnessPolicy() string {
	const common = "indexed: the projection is rebuilt whole whenever a store file's size or " +
		"modification time changes, checked before every search and on recall/refresh. Clara " +
		"rewrites these files in place rather than appending, so a byte cursor would miss a " +
		"rewrite and keep serving deleted records"
	if k == StoreMemory {
		return common + "; archived memory stays searchable and ranks below live records"
	}
	return common + "; a parse failure in observations.jsonl reports partial, because a signal " +
		"shown without an action it has is a false absence, while an absent observations.jsonl " +
		"means nothing has been acted on and is complete"
}

// files returns the store's files in scan order, with whether each one has to
// be there. A missing required file is a source that cannot be read; a missing
// optional one is a corpus that has not written it yet.
func (k storeKind) files(dir string) []storeFile {
	if k == StoreMemory {
		return []storeFile{
			{path: filepath.Join(dir, fileMemory), role: roleLive},
			{path: filepath.Join(dir, fileMemoryArchive), role: roleArchive},
		}
	}
	return []storeFile{
		{path: filepath.Join(dir, fileSignals), role: roleLive},
		{path: filepath.Join(dir, fileSignalsArchive), role: roleArchive},
		{path: filepath.Join(dir, fileObservations), role: roleObservations},
	}
}

// Close releases the adapter. Later calls must fail rather than answer from a
// projection nobody is maintaining any more.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.ready = false
	a.snap = nil
	return nil
}

// session snapshots what one request needs, or names the reason there is none.
// Holding the read lock for a whole search would serialize searches against a
// projection that is immutable once published.
func (a *Adapter) session() (session, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.closed:
		return session{}, adapter.ErrClosed
	case !a.ready:
		return session{}, ErrNotInitialized
	}
	return session{
		set: a.set, store: a.store, sourceID: a.sourceID,
		floor: a.floor, loc: a.loc,
	}, nil
}

// session is the instance policy one request reads. It is a value, so nothing
// downstream reaches back into the adapter and retakes a lock per candidate.
type session struct {
	set      Settings
	store    storeKind
	sourceID string
	floor    recall.Sensitivity
	loc      *time.Location
}

func (a *Adapter) now() time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	return time.Now().UTC()
}

// current returns a generation built from the store as it is now, rebuilding
// and publishing a new one when any file has changed.
func (a *Adapter) current(ctx context.Context, full bool) (*snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := a.session()
	if err != nil {
		return nil, err
	}
	a.buildMu.Lock()
	defer a.buildMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	a.mu.RLock()
	prev, dir, gen, workdir := a.snap, a.dir, a.gen, a.workdir
	a.mu.RUnlock()

	files := s.store.files(dir)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := a.now()
	if prev != nil && !full && !changed(files, prev.files) {
		switch {
		case s.store != StoreMemory:
			return prev, nil
		case s.set.Today != "":
			// A pinned date is intentionally stable for conformance and eval.
			return prev, nil
		case prev.today == civilOf(now.In(s.loc)):
			return prev, nil
		}
		// Memory effective weight is a function of Clara's civil today. Crossing
		// midnight in the corpus timezone changes scores, ordering, metadata,
		// and fingerprints even when no JSONL byte changed.
	}

	next, err := build(ctx, files, s, gen+1, now)
	if err != nil {
		// The generation already published stays published: it is the one still
		// answering. Callers see the failure and the older generation.
		return nil, err
	}

	// The generation identity becomes durable before the generation becomes
	// visible. If checkpointing fails, the previous immutable snapshot keeps
	// answering and the changed content is never exposed under an id a
	// restarted process could reuse.
	cp := checkpoint{
		Generation: next.gen,
		UpdatedAt:  next.builtAt,
		Watermark:  next.watermark(),
		Files:      next.files,
	}
	err = saveCheckpoint(workdir, cp)
	if err != nil {
		a.mu.Lock()
		a.cpFailed = true
		a.mu.Unlock()
		return nil, protocol.Errorf(protocol.CodeSourceUnavailable,
			"clara-corpus: checkpoint generation %d: %v", next.gen, err)
	}

	a.mu.Lock()
	a.snap, a.gen, a.prior = next, next.gen, cp
	a.cpFailed = false
	a.mu.Unlock()
	return next, nil
}

// Health probes the store. A failed probe still reports the generation still
// published, because that is the one still answering.
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	snap, err := a.current(ctx, false)
	if err != nil {
		return a.degraded(err), nil
	}
	return a.healthOf(snap), nil
}

// Refresh rebuilds the projection and reports the resulting health. This is
// what the checkpoint capability means.
func (a *Adapter) Refresh(ctx context.Context, p protocol.RefreshParams) (recall.Health, error) {
	if !p.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, p.Deadline)
		defer cancel()
	}
	snap, err := a.current(ctx, p.Full)
	if err != nil {
		// Both the error and the health of the generation still published.
		return a.degraded(err), err
	}
	return a.healthOf(snap), nil
}

func (a *Adapter) healthOf(snap *snapshot) recall.Health {
	a.mu.RLock()
	cpFailed, identity, store, loc := a.cpFailed, a.identity, a.store, a.loc
	a.mu.RUnlock()

	h := recall.Health{
		Status:          recall.HealthHealthy,
		CheckedAt:       a.now(),
		LastSuccess:     &snap.builtAt,
		SourceWatermark: snap.watermark(),
		IndexWatermark:  snap.watermark(),
		IndexGeneration: snap.generation(),
		IndexConfig:     IndexConfig,
		RecordCount:     int64(len(snap.items) + snap.failed),
		IndexedCount:    int64(len(snap.items)),
		FailedCount:     int64(snap.failed + snap.obsFailed),
		Coverage:        snap.coverage(),
		Diagnostics: map[string]any{
			// The store this instance opened. `recall doctor` refuses a profile
			// in which two enabled instances of one adapter report one value;
			// see protocol.DiagStoreIdentity.
			protocol.DiagStoreIdentity: identity,
			"store":                    string(store),
			"live_records":             snap.live,
			"archived_records":         snap.archived,
			// The zone the civil dates were read in. Decay ages whole days from
			// a civil date, so this is part of the answer, not trivia.
			"decay_timezone": loc.String(),
		},
	}
	if store == StoreSignals {
		h.Diagnostics["observations"] = len(snap.obs)
		h.Diagnostics["signals_with_action"] = snap.withAction
	}
	if store == StoreMemory {
		// The date the decay arithmetic was measured against. Reported because
		// every effective weight in every answer depends on it, and because a
		// pinned date has to be visible rather than inferred from a suspiciously
		// stable result.
		h.Diagnostics["aged_to"] = snap.today.String()
		if snap.today != civilOf(h.CheckedAt.In(loc)) {
			h.Diagnostics["aged_to_pinned"] = true
		}
	}
	if len(snap.absent) > 0 {
		// Named, not counted: "the archive has never been written" and "nothing
		// has been acted on" are different facts about a corpus, and both are
		// normal. Absence of an optional store is not partial coverage.
		h.Diagnostics["absent_files"] = snap.absent
	}
	if snap.failed > 0 {
		h.Diagnostics["failed_lines"] = snap.failed
	}
	if snap.obsFailed > 0 {
		h.Diagnostics["failed_observation_lines"] = snap.obsFailed
	}
	if snap.duplicates > 0 {
		h.Diagnostics["duplicate_records_resolved"] = snap.duplicates
	}
	if len(snap.schemas) > 0 {
		h.Diagnostics["schema_versions"] = snap.schemas
	}
	if cpFailed {
		// The workdir is the one place this adapter may write. Losing it does
		// not corrupt an answer, but it does mean the next process starts with
		// no record of what this one read.
		h.Diagnostics["checkpoint_unwritable"] = true
		h.Status = recall.HealthDegraded
	}
	if snap.coverage() != recall.IndexComplete {
		// Partial coverage is reported as degraded rather than healthy: the
		// freshness policy declares no partial boundary acceptable, and a
		// recent index alone is not health.
		h.Status = recall.HealthDegraded
	}
	return h
}

// degraded renders a failed scan without losing what is still published.
func (a *Adapter) degraded(err error) recall.Health {
	h := adapter.Unhealthy(err)
	a.mu.RLock()
	snap, prior, identity, cpFailed := a.snap, a.prior, a.identity, a.cpFailed
	a.mu.RUnlock()
	if identity != "" {
		if h.Diagnostics == nil {
			h.Diagnostics = map[string]any{}
		}
		// Reported even when the store could not be read: an unreachable
		// instance still names which store it was configured to own, and
		// doctor's isolation check has to see both halves of a collision.
		h.Diagnostics[protocol.DiagStoreIdentity] = identity
	}
	if cpFailed {
		if h.Diagnostics == nil {
			h.Diagnostics = map[string]any{}
		}
		h.Diagnostics["checkpoint_unwritable"] = true
	}
	switch {
	case snap != nil:
		h.IndexGeneration = snap.generation()
		h.IndexWatermark = snap.watermark()
		h.IndexedCount = int64(len(snap.items))
		h.LastSuccess = &snap.builtAt
	case prior.Generation > 0:
		h.IndexGeneration = fmt.Sprintf("gen-%d", prior.Generation)
		h.IndexWatermark = prior.Watermark
		h.LastSuccess = &prior.UpdatedAt
	}
	return h
}

// parseSettings decodes and validates the settings block. Unknown keys are
// rejected: a misspelled setting that silently did nothing would be
// configuration with no code path behind it, which docs/spec.md calls a defect
// rather than a tolerance.
func parseSettings(raw map[string]any) (Settings, error) {
	var set Settings
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "clara-corpus settings: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "clara-corpus settings: %v", err)
	}
	if !knownStore(set.Store) {
		// Required, with no default. A corpus holds both stores, so guessing
		// would answer a question about memory out of the signal stream.
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus settings: store must be one of %s, got %q", storeNames(), set.Store)
	}
	for source, sourceID := range set.Upstream {
		if source == "" || sourceID == "" || strings.Contains(sourceID, ":") {
			// A source_id containing the locator separator would produce an edge
			// that parses as a different source entirely.
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"clara-corpus settings: upstream %q maps to an unusable source_id", source)
		}
	}
	if set.MaxCandidates < 0 || set.StallMS < 0 {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus settings: max_candidates and debug_stall_ms cannot be negative")
	}
	if set.Today != "" {
		if storeKind(set.Store) != StoreMemory {
			// Only memory ages anything. Accepting the key here would be
			// configuration with no code path behind it, which docs/spec.md
			// calls a defect.
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"clara-corpus settings: debug_today applies to the memory store only; "+
					"nothing in the %s store is aged", set.Store)
		}
		if parseCivil(set.Today).zero() {
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"clara-corpus settings: debug_today %q is not a YYYY-MM-DD date", set.Today)
		}
	}
	return set, nil
}

func knownStore(name string) bool {
	for _, k := range storeKinds {
		if string(k) == name {
			return true
		}
	}
	return false
}

func storeNames() string {
	names := make([]string, 0, len(storeKinds))
	for _, k := range storeKinds {
		names = append(names, string(k))
	}
	return strings.Join(names, ", ")
}

func (s Settings) maxCandidates() int {
	if s.MaxCandidates > 0 {
		return s.MaxCandidates
	}
	return DefaultMaxCandidates
}

// zone resolves the corpus timezone. An unknown name is refused at the
// handshake rather than silently falling back to UTC: decay ages whole days
// from a civil date, so a zone nobody loaded would move every boundary by up to
// a day with nothing in the answer saying so.
func zone(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus settings: timezone %q is not a known IANA zone", name)
	}
	return loc, nil
}

// resolveDataDir turns the configured location into the Clara data directory
// this instance opens.
//
// A corpus root and its data/ subdirectory both name one store, so both are
// accepted and both resolve to the same path — which is the whole point, since
// that resolved path is what store_identity compares. Symlinks are evaluated
// for the same reason: two configurations reaching one directory by different
// names are one store, and only the resolved path says so.
func resolveDataDir(location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus: this source has no location, so there is no corpus to read")
	}
	dir := filepath.Clean(location)
	if !isClaraData(dir) && isClaraData(filepath.Join(dir, "data")) {
		dir = filepath.Join(dir, "data")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", protocol.Errorf(protocol.CodeSourceUnavailable,
			"clara-corpus: %q is not a readable directory", location)
	}
	if !isClaraData(dir) {
		// A directory that exists and holds no Clara store is almost certainly
		// a mistyped path. Refusing here is loud; the alternative is a source
		// that reports complete coverage over nothing.
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"clara-corpus: %q holds no Clara store files, so it is not a corpus data directory", location)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir, nil
}

func isClaraData(dir string) bool {
	for _, name := range claraMarkers {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// settingsSchema declares the settings block. `recall doctor` validates a
// configuration against it without starting a query, so every key here is one
// the code above reads.
func settingsSchema() map[string]any {
	stores := make([]any, 0, len(storeKinds))
	for _, k := range storeKinds {
		stores = append(stores, string(k))
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"store"},
		"properties": map[string]any{
			"store": map[string]any{
				"type": "string", "enum": stores,
				"description": "Which Clara store this instance serves. Required: a corpus holds both, " +
					"and they are different sources with different authority and different priors.",
			},
			"upstream": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Maps a Clara source name to the source_id of the Recall source owning " +
					"those records. Each mapped signal emits derived_from '<source_id>:<native id>'; " +
					"an unmapped source emits no edge.",
			},
			"timezone": map[string]any{
				"type": "string",
				"description": "IANA zone Clara computes its civil dates in; match `timezone` in " +
					"Clara's config. Empty means UTC. Decay ages whole days from a civil date.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Cap on candidates returned by one search.",
			},
			"debug_today": map[string]any{
				"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`,
				"description": "Fixed civil date to age memory records from, so a decay transcript and an " +
					"evaluation pack replay identically. Memory store only. Leave unset in real configuration.",
			},
			"debug_stall_ms": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Artificial pre-scan delay, for recording the cancellation conformance case. Leave unset in real configuration.",
			},
		},
	}
}

// stall delays a search and returns as soon as the context is done.
//
// This is the whole of what an adapter owes a cancellation: notice, return, and
// do not answer. The core's recall/cancel cancels the request context, and the
// server turns the returned context error into a protocol error rather than an
// empty success.
func stall(ctx context.Context, ms int) error {
	if ms <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ adapter.Adapter = (*Adapter)(nil)
