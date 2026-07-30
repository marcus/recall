package stream

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

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Adapter identity and defaults.
const (
	// AdapterID is this implementation's identity in manifests and reports.
	AdapterID = "recall-stream/1"

	// DisplayName is the adapter's name. The instance's name is its
	// configured source_id and arrives at the handshake.
	DisplayName = "JSONL Stream"

	// IndexConfig identifies the retrieval configuration a generation was
	// built under. It changes whenever tokenization or scoring changes, so an
	// evaluation comparing two generations cannot mistake a scoring change for
	// the change under test.
	IndexConfig = "jsonl/1 tokenizer=ident-runs scoring=term-coverage"

	// DefaultMaxCandidates caps one search's candidate list so a long stream
	// cannot flood the fusion pool. The core's per-source limit narrows it
	// further.
	DefaultMaxCandidates = 50

	// checkpointFile is the only file this adapter writes, and it is written
	// only inside the handshake's workdir.
	checkpointFile = "cursor.json"
)

// Settings is the adapter-owned settings block, declared by
// [recall.Manifest.SettingsSchema] and validated here on every handshake.
type Settings struct {
	// Files are the stream files, absolute or relative to the configured
	// location. Empty means the location itself names the single file.
	Files []string `json:"files"`

	// Upstream maps a record's `system` to the source_id of the Recall source
	// that owns those records. It is what turns a normalized event into a
	// derived_from edge; a system absent here yields no edge.
	Upstream map[string]string `json:"upstream"`

	MaxCandidates int `json:"max_candidates"`

	// StallMS delays every search before it scans. It exists so the
	// cancellation conformance case can be recorded deterministically against
	// a real process, and it is the shortest demonstration of the only thing
	// an adapter owes a cancel: notice the context, return, do not answer.
	// Leave it unset in real configuration.
	StallMS int `json:"debug_stall_ms"`
}

// Options are construction-time seams. Instance policy arrives at the
// handshake, because that is where the spec puts it.
type Options struct {
	// Clock supplies build and probe timestamps. Nil means [time.Now].
	Clock func() time.Time
}

// Adapter is the JSONL stream source.
//
// The zero value is not usable; build one with [New]. It holds a published
// index generation and the cursors that produced it.
type Adapter struct {
	opts Options

	mu       sync.RWMutex
	ready    bool
	closed   bool
	sourceID string
	workdir  string
	files    []string
	set      Settings
	floor    recall.Sensitivity
	gen      int64
	prior    checkpoint
	snap     *snapshot
	cpFailed bool

	// buildMu serializes scans. Two concurrent searches must not both walk the
	// files; one builds, the other reads the generation it publishes.
	buildMu sync.Mutex
}

// New returns an uninitialized adapter.
func New(opts Options) *Adapter { return &Adapter{opts: opts} }

// ErrNotInitialized reports use before a successful handshake. It carries the
// source_unavailable code, so errors.Is against the sentinel still matches,
// while keeping a message the wire can show.
var ErrNotInitialized = protocol.Errorf(protocol.CodeSourceUnavailable,
	"stream adapter has not completed a handshake")

// Initialize negotiates the protocol version, validates the settings block,
// resolves the stream files, and adopts the workdir's checkpoint. It reads no
// stream bytes: building an index inside the handshake competes with the
// handshake timeout, which is why recall/refresh exists.
func (a *Adapter) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
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
	files, err := resolveFiles(cfg.Location, set.Files)
	if err != nil {
		return recall.Manifest{}, err
	}
	if strings.TrimSpace(cfg.Workdir) == "" {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeInvalidParams,
			"stream: handshake supplied no workdir, and this adapter has nowhere else it may write")
	}

	prior := loadCheckpoint(cfg.Workdir)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.sourceID = cfg.SourceID
	a.workdir = cfg.Workdir
	a.files = files
	a.set = set
	a.floor = recall.SensitivityInternal
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
		RecordTypes:     []recall.RecordType{recall.RecordEvent, recall.RecordMessage},
		QueryModes: []recall.QueryMode{
			recall.QueryExact, recall.QueryLexical, recall.QueryTemporal,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},

		// Every record carries an immutable event_time, so a boundary can be
		// filtered for. Reconstructing the file's contents at a past instant
		// cannot be done from an event time, so snapshot is refused. See the
		// package doc.
		AsOfSupport:    recall.AsOfFilter,
		RelevanceBasis: recall.RelevanceLexicalSpan,

		Capabilities: []recall.Capability{
			recall.CapSearch, recall.CapExpand, recall.CapCheckpoint,
		},
		FreshnessPolicy: "indexed: the projection is caught up by byte offset before every " +
			"search and on recall/refresh; a shortened file forces a full rebuild",
		Sensitivity:    recall.SensitivityInternal,
		SettingsSchema: settingsSchema(),
	}, nil
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
func (a *Adapter) session() (Settings, string, recall.Sensitivity, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.closed:
		return Settings{}, "", 0, adapter.ErrClosed
	case !a.ready:
		return Settings{}, "", 0, ErrNotInitialized
	}
	return a.set, a.sourceID, a.floor, nil
}

func (a *Adapter) now() time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	return time.Now().UTC()
}

// current returns a generation that has consumed every complete line the files
// hold, building and publishing a new one when they have grown.
func (a *Adapter) current(full bool) (*snapshot, error) {
	if _, _, _, err := a.session(); err != nil {
		return nil, err
	}
	a.buildMu.Lock()
	defer a.buildMu.Unlock()

	a.mu.RLock()
	prev, paths, gen, workdir, prior := a.snap, a.files, a.gen, a.workdir, a.prior.Files
	a.mu.RUnlock()

	if prev != nil && !full && !grown(paths, prev.files) {
		return prev, nil
	}

	next, err := build(paths, prev, prior, gen+1, full, a.now())
	if err != nil {
		// The generation already published stays published: it is the one
		// still answering. Callers see the failure and the older generation.
		return nil, err
	}

	a.mu.Lock()
	a.snap, a.gen = next, next.gen
	a.mu.Unlock()

	// Durable first, checkpoint second. Here "durable" is the published
	// generation; an adapter with an on-disk index would write its records
	// before reaching this line.
	//
	// The outcome is recorded on the adapter rather than on the snapshot,
	// which is already published and therefore already being read: a
	// generation is immutable once it is answering.
	err = saveCheckpoint(workdir, checkpoint{
		Generation: next.gen,
		UpdatedAt:  next.builtAt,
		Watermark:  next.watermark(),
		Files:      next.files,
	})
	a.mu.Lock()
	// A generation that cannot be checkpointed still answers correctly; it
	// only loses the ability to tell the next process what it consumed.
	a.cpFailed = err != nil
	a.mu.Unlock()
	return next, nil
}

// grown reports whether any file holds bytes past its cursor. A stream that
// only ever grows makes this the whole of change detection.
func grown(paths []string, cursors []fileCursor) bool {
	byPath := make(map[string]fileCursor, len(cursors))
	for _, c := range cursors {
		byPath[c.Path] = c
	}
	for _, path := range paths {
		st, err := os.Stat(path)
		cur, seen := byPath[path]
		switch {
		case err != nil:
			// Missing now: either it always was, or it disappeared. Rescanning
			// is how the difference gets reported.
			return !seen || !cur.Missing
		case st.Size() != cur.Offset:
			return true
		}
	}
	return false
}

// Health probes the source. A failed probe still reports the generation still
// published, because that is the one still answering.
func (a *Adapter) Health(context.Context) (recall.Health, error) {
	snap, err := a.current(false)
	if err != nil {
		return a.degraded(err), nil
	}
	return a.healthOf(snap), nil
}

// Refresh brings the projection up to date and reports the resulting health.
// This is what the checkpoint capability means.
func (a *Adapter) Refresh(_ context.Context, p protocol.RefreshParams) (recall.Health, error) {
	snap, err := a.current(p.Full)
	if err != nil {
		// Both the error and the health of the generation still published.
		return a.degraded(err), err
	}
	return a.healthOf(snap), nil
}

func (a *Adapter) healthOf(snap *snapshot) recall.Health {
	now := a.now()
	a.mu.RLock()
	cpFailed := a.cpFailed
	a.mu.RUnlock()
	h := recall.Health{
		Status:          recall.HealthHealthy,
		CheckedAt:       now,
		LastSuccess:     &snap.builtAt,
		SourceWatermark: snap.watermark(),
		IndexWatermark:  snap.watermark(),
		IndexGeneration: snap.generation(),
		IndexConfig:     IndexConfig,
		RecordCount:     int64(len(snap.records) + snap.failed),
		IndexedCount:    int64(len(snap.records)),
		FailedCount:     int64(snap.failed),
		Coverage:        snap.coverage(),
		Diagnostics:     map[string]any{"files": len(snap.files)},
	}
	if len(snap.missing) > 0 {
		h.Diagnostics["missing_files"] = snap.missing
	}
	if cpFailed {
		// The workdir is the one place this adapter may write. Losing it does
		// not corrupt an answer, but it does mean the next process starts with
		// no record of what this one consumed.
		h.Diagnostics["checkpoint_unwritable"] = true
		h.Status = recall.HealthDegraded
	}
	if snap.rewritten {
		// An append-only stream that shrank was rewritten. The projection is
		// correct — it was rebuilt — but the source broke its own contract, and
		// a caller comparing watermarks across that boundary deserves to know.
		h.Diagnostics["stream_rewritten"] = true
	}
	if snap.coverage() != recall.IndexComplete || snap.rewritten {
		// Partial coverage is reported as degraded rather than healthy: no
		// freshness policy here declares this exact partial boundary
		// acceptable, and a recent index alone is not health.
		h.Status = recall.HealthDegraded
	}
	return h
}

// degraded renders a failed scan without losing what is still published.
func (a *Adapter) degraded(err error) recall.Health {
	h := adapter.Unhealthy(err)
	a.mu.RLock()
	snap, prior := a.snap, a.prior
	a.mu.RUnlock()
	if snap != nil {
		h.IndexGeneration = snap.generation()
		h.IndexWatermark = snap.watermark()
		h.IndexedCount = int64(len(snap.records))
		h.LastSuccess = &snap.builtAt
		return h
	}
	if prior.Generation > 0 {
		h.IndexGeneration = fmt.Sprintf("gen-%d", prior.Generation)
		h.IndexWatermark = prior.Watermark
		h.LastSuccess = &prior.UpdatedAt
	}
	return h
}

// parseSettings decodes and validates the settings block. Unknown keys are
// rejected: a misspelled setting that silently did nothing would be
// configuration with no code path behind it.
func parseSettings(raw map[string]any) (Settings, error) {
	var set Settings
	if len(raw) == 0 {
		return set, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "stream settings: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "stream settings: %v", err)
	}
	for system, sourceID := range set.Upstream {
		if system == "" || sourceID == "" || strings.Contains(sourceID, ":") {
			// A source_id containing the locator separator would produce an
			// edge that parses as a different source entirely.
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"stream settings: upstream %q maps to an unusable source_id", system)
		}
	}
	if set.MaxCandidates < 0 || set.StallMS < 0 {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"stream settings: max_candidates and debug_stall_ms cannot be negative")
	}
	return set, nil
}

func (s Settings) maxCandidates() int {
	if s.MaxCandidates > 0 {
		return s.MaxCandidates
	}
	return DefaultMaxCandidates
}

// resolveFiles turns a location plus configured names into absolute paths.
//
// A relative name may not climb out of the location. Settings are adapter-owned
// and unvalidated when configuration is loaded, so this is the layer that has
// to refuse "../../.ssh/id_rsa" — nothing above it will.
func resolveFiles(location string, names []string) ([]string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"stream: this source has no location, so there is no stream to read")
	}
	if len(names) == 0 {
		return []string{filepath.Clean(location)}, nil
	}
	base := filepath.Clean(location)
	if st, err := os.Stat(base); err == nil && !st.IsDir() {
		base = filepath.Dir(base)
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if filepath.IsAbs(name) {
			out = append(out, filepath.Clean(name))
			continue
		}
		full := filepath.Join(base, name)
		rel, err := filepath.Rel(base, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, protocol.Errorf(protocol.CodeInvalidParams,
				"stream settings: file %q resolves outside the configured location", name)
		}
		out = append(out, full)
	}
	return out, nil
}

// settingsSchema declares the settings block. `recall doctor` validates a
// configuration against it without starting a query, so every key here is one
// the code above reads.
func settingsSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"files": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Stream files, absolute or relative to the location. Empty means the location is the file.",
			},
			"upstream": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "Maps a record's `system` to the source_id owning those records. " +
					"Each mapped record emits derived_from '<source_id>:<ref>'; an unmapped system emits no edge.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Cap on candidates returned by one search.",
			},
			"debug_stall_ms": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Artificial pre-scan delay, for recording the cancellation conformance case. Leave unset in real configuration.",
			},
		},
	}
}

var _ adapter.Adapter = (*Adapter)(nil)
