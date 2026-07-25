package docs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Adapter identity, reported in the manifest.
const (
	adapterID   = "recall-docs/0.1.0"
	displayName = "Documents"

	// freshnessPolicy states the exact partial boundary this source may report
	// as healthy, which is what makes "healthy" checkable rather than a claim.
	freshnessPolicy = "indexed: one generation published atomically per complete corpus walk; " +
		"stale or partial generations are reported degraded and keep answering"
)

// Adapter is a document corpus source: Markdown chunked by heading, ranked by
// BM25 from an adapter-owned index, expanded live from the file.
//
// One Adapter owns one workdir. Concurrent searches and expansions are safe:
// the published generation is immutable, and a rebuild swaps a pointer rather
// than mutating what a search is reading.
type Adapter struct {
	// buildMu serializes builds. It is separate from mu so a build never holds
	// the lock that searches need: an index rebuild must not stop the previous
	// generation from answering.
	buildMu sync.Mutex

	mu           sync.RWMutex
	closed       bool
	sourceID     string
	root         string
	indexDir     string
	settings     Settings
	gen          *generation
	lastSuccess  *time.Time
	lastBuildErr string
}

// New returns an unconfigured document adapter. Everything it needs — corpus
// root, workdir, settings, source name — arrives at the handshake, because a
// built-in adapter and an external one must be configurable the same way.
func New() *Adapter { return &Adapter{} }

var _ adapter.Adapter = (*Adapter)(nil)

// Initialize negotiates the protocol version, validates the settings block, and
// makes sure a generation exists.
//
// The first handshake builds one synchronously. The alternative is a source
// that answers "unavailable" until something else happens to trigger a build,
// which is indistinguishable from a broken source to everyone downstream.
func (a *Adapter) Initialize(ctx context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	settings, err := parseSettings(cfg.Settings)
	if err != nil {
		return recall.Manifest{}, err
	}

	root := settings.Root
	if root == "" {
		root = cfg.Location
	}
	if root == "" {
		return recall.Manifest{}, errors.New("docs: no corpus: set the source location or settings.root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return recall.Manifest{}, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeSourceUnavailable,
			"corpus root %s is not a readable directory", filepath.Base(root))
	}

	if cfg.Workdir == "" {
		return recall.Manifest{}, errors.New("docs: no workdir: an index has nowhere to live")
	}
	indexDir := filepath.Join(cfg.Workdir, "index")
	if err := os.MkdirAll(indexDir, genDirPerm); err != nil {
		return recall.Manifest{}, fmt.Errorf("docs: workdir: %w", err)
	}

	a.mu.Lock()
	a.closed = false
	a.sourceID = cfg.SourceID
	a.root = root
	a.indexDir = indexDir
	a.settings = settings
	a.mu.Unlock()

	gen, err := openIndex(indexDir)
	switch {
	case err == nil && gen.header.Root != root:
		// The workdir holds an index of a different corpus. Answering from it
		// would attribute one source's documents to another.
		gen = nil
	case err != nil && !errors.Is(err, errNoGeneration):
		// A corrupt or truncated generation is not a reason to refuse service:
		// it is a reason to rebuild. It is reported either way.
		a.setBuildError(err)
		gen = nil
	}
	if gen != nil {
		a.mu.Lock()
		a.gen = gen
		built := gen.header.BuiltAt
		a.lastSuccess = &built
		a.mu.Unlock()
	} else if _, err := a.Refresh(ctx, protocol.RefreshParams{}); err != nil {
		return recall.Manifest{}, err
	}

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       adapterID,
		DisplayName:     displayName,
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		// Exact is declared because a path or a declared alias is looked up as
		// an identifier, not scored as text. Semantic is not declared: this
		// adapter is lexical, and a mode it cannot serve would make it eligible
		// for requests it would answer badly.
		QueryModes:     []recall.QueryMode{recall.QueryLexical, recall.QueryExact},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
		// filter, never snapshot: as_of is honored by excluding documents
		// modified after the boundary. Their earlier text is not recoverable
		// from the filesystem, and pretending otherwise would answer a
		// historical question from current state.
		AsOfSupport:     recall.AsOfFilter,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		FreshnessPolicy: freshnessPolicy,
		Sensitivity:     recall.SensitivityInternal,
		SettingsSchema:  settingsSchema(),
	}, nil
}

// Refresh indexes the corpus into a new generation and publishes it.
//
// Indexing is an operation someone schedules — at handshake, from a maintenance
// command, after an edit — and a build that can only be triggered as a side
// effect of something else cannot be tested for what happens when it fails.
//
// A build that fails is reported through the returned health and not as an
// error. That is not softening the failure: health carries the stale watermark,
// a non-healthy status, and the reason in diagnostics, which is strictly more
// than an error conveys. The error return means the refresh could not run.
func (a *Adapter) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	a.buildMu.Lock()
	defer a.buildMu.Unlock()

	a.mu.RLock()
	root, settings, indexDir, closed := a.root, a.settings, a.indexDir, a.closed
	a.mu.RUnlock()
	if closed {
		return recall.Health{}, adapter.ErrClosed
	}
	if indexDir == "" {
		return recall.Health{}, errors.New("docs: build before handshake")
	}

	gen, err := buildIndex(ctx, root, settings, indexDir)
	if err != nil {
		// Nothing was published. The previously published generation is still
		// the current one, still readable, and health now reports why it is not
		// moving forward.
		a.setBuildError(err)
		// Reported through health rather than returned as an error: a JSON-RPC
		// frame carries a result or an error and never both, so an error return
		// would discard the health of the generation that is still published
		// and still answering. The error return is reserved for a failure to
		// perform the refresh at all.
		return a.Health(context.WithoutCancel(ctx))
	}

	built := gen.header.BuiltAt
	a.mu.Lock()
	a.gen = gen
	a.lastSuccess = &built
	a.lastBuildErr = ""
	a.mu.Unlock()
	// The build is done either way, so its report must not fail because the
	// caller's context expired between the publication and the probe.
	return a.Health(context.WithoutCancel(ctx))
}

func (a *Adapter) setBuildError(err error) {
	a.mu.Lock()
	a.lastBuildErr = err.Error()
	a.mu.Unlock()
}

// Search ranks chunks against the query, from the published generation only.
//
// It does not stat the corpus. Staleness is a property of the source over time
// and belongs to Health, which the core probes on its own cadence; walking the
// corpus on every query would make every search pay for a fact that changes far
// more slowly than queries arrive.
func (a *Adapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	a.mu.RLock()
	gen, sourceID, closed := a.gen, a.sourceID, a.closed
	a.mu.RUnlock()

	switch {
	case closed:
		return recall.SearchResponse{Outcome: recall.SearchUnavailable}, adapter.ErrClosed
	case gen == nil:
		return recall.SearchResponse{Outcome: recall.SearchUnavailable},
			protocol.Errorf(protocol.CodeSourceUnavailable, "no published index generation")
	}
	if err := expired(ctx, req.Deadline); err != nil {
		return recall.SearchResponse{Outcome: recall.SearchTimeout}, err
	}

	start := time.Now()
	hits := searchIndex(gen, req)
	found := candidates(gen, sourceID, hits, req.Limit)

	// A generation built over a partial boundary answers partial, every time it
	// answers. The alternative is a source reporting success over a corpus it
	// only partly read.
	outcome := recall.SearchSuccess
	if gen.coverage != recall.IndexComplete {
		outcome = recall.SearchPartial
	}
	return recall.SearchResponse{
		Candidates:      found,
		Diagnostics:     searchDiagnostics(gen, req, len(hits), time.Since(start)),
		SourceWatermark: gen.header.Watermark,
		Outcome:         outcome,
	}, nil
}

// Expand retrieves evidence behind a locator, live from the file.
func (a *Adapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	a.mu.RLock()
	gen, root, closed := a.gen, a.root, a.closed
	a.mu.RUnlock()

	switch {
	case closed:
		return recall.ExpandResponse{}, adapter.ErrClosed
	case gen == nil:
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeSourceUnavailable,
			"no published index generation")
	}
	if err := expired(ctx, req.Deadline); err != nil {
		return recall.ExpandResponse{}, err
	}
	return expand(gen, root, req)
}

// expired reports a request that is already out of time.
//
// Both bounds are honored: the context, which the core cancels, and the
// deadline the request itself carries, which is what an external adapter would
// see on the wire. Starting work that cannot finish would spend the caller's
// remaining budget to return nothing.
func expired(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// Health reports the index against the live corpus.
//
// This is the one operation that walks the corpus, because it is the only one
// whose answer depends on what the source looks like NOW rather than on what
// the last complete boundary saw. A recent build is not health: the watermarks
// are what say whether the published generation still describes the corpus.
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	a.mu.RLock()
	gen, root, settings, closed := a.gen, a.root, a.settings, a.closed
	lastSuccess, buildErr := a.lastSuccess, a.lastBuildErr
	a.mu.RUnlock()

	if closed {
		return recall.Health{}, adapter.ErrClosed
	}
	if err := expired(ctx, time.Time{}); err != nil {
		return recall.Health{}, err
	}
	health := recall.Health{
		Status:      recall.HealthUnavailable,
		CheckedAt:   time.Now().UTC(),
		LastSuccess: lastSuccess,
		Coverage:    recall.IndexUnknown,
		Diagnostics: map[string]any{},
	}
	if buildErr != "" {
		health.Diagnostics["last_build_error"] = buildErr
	}
	if gen != nil {
		health.IndexWatermark = gen.header.Watermark
		health.IndexGeneration = gen.id
		health.IndexedCount = int64(len(gen.docs))
		health.FailedCount = int64(len(gen.failures))
		health.Coverage = gen.coverage
		health.Diagnostics["chunk_count"] = len(gen.chunks)
		if gen.header.GitRevision != "" {
			health.Diagnostics["indexed_git_revision"] = gen.header.GitRevision
		}
		if len(gen.failures) > 0 {
			health.Diagnostics["failures"] = failureSummary(gen.failures)
		}
	}

	corpus, err := scanCorpus(root, settings)
	if err != nil {
		// The index can still answer, but nothing can confirm it describes the
		// corpus, and expansion reads files this walk could not see.
		health.Diagnostics["scan_error"] = err.Error()
		if gen != nil {
			health.Status = recall.HealthDegraded
			health.Coverage = recall.IndexUnknown
		}
		return health, nil
	}

	health.SourceWatermark = corpus.Watermark
	health.RecordCount = int64(len(corpus.Files))
	if corpus.GitRevision != "" {
		health.Diagnostics["git_revision"] = corpus.GitRevision
	}
	if gen == nil {
		health.Diagnostics["reason"] = "no published index generation"
		return health, nil
	}

	stale := corpus.Watermark != gen.header.Watermark
	health.Diagnostics["stale"] = stale
	switch {
	case stale:
		// Records exist in the corpus that the index does not represent. That
		// is exactly the partial boundary the freshness policy permits, and it
		// is reported as degraded rather than healthy so nobody reads a
		// published generation as a current one.
		health.Status = recall.HealthDegraded
		health.Coverage = recall.IndexPartial
	case len(gen.failures) > 0 || gen.coverage != recall.IndexComplete:
		health.Status = recall.HealthDegraded
	default:
		health.Status = recall.HealthHealthy
	}
	return health, nil
}

// failureSummary keeps the health report bounded. The generation retains every
// failure; a probe reports the count and a readable sample.
func failureSummary(failures []indexFailure) []string {
	const sample = 5
	out := make([]string, 0, min(sample, len(failures)))
	for _, f := range failures[:min(sample, len(failures))] {
		out = append(out, f.Path+": "+f.Reason)
	}
	return out
}

// Close releases the adapter. The published generation stays on disk: it is a
// projection the next handshake reopens, not state that belongs to a process.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.gen = nil
	return nil
}
