package ongoing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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
	AdapterID = "recall-ongoing/1"

	// DisplayName is the adapter's name. The instance's name is its configured
	// source_id and arrives at the handshake.
	DisplayName = "Ongoing Projects"

	// RecordProject is the record type every candidate carries.
	//
	// docs/adapter-protocol.md declares the record type set open — "person |
	// task | document | message | event | ..." — and both the Go type and the
	// wire schema honor that: recall.RecordType is a bare string and
	// common.json#/$defs/record_type is `{"type":"string","minLength":1}`. A
	// catalogued repository is none of the named five: it is not a task, and
	// calling it a document would flatten a structured record into prose the
	// moment anything filtered on type. So it declares its own, and no core
	// change was needed to accept it.
	RecordProject recall.RecordType = "project"

	// DefaultTimeout bounds one API call. The catalog is a local SQLite read
	// behind an HTTP handler on the LAN; anything approaching this is a wedged
	// instance, not slow work.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxCandidates caps one search's candidate list so a large scan
	// root cannot flood the fusion pool. The core's per-source limit narrows it
	// further.
	DefaultMaxCandidates = 25
)

// Sentinels. Both are protocol errors so errors.Is keeps matching after a
// wrapper adds detail, and so the code reaches the wire unchanged.
var (
	errUnavailable error = protocol.ErrSourceUnavailable
	errDenied      error = protocol.ErrSourceDenied
)

// ErrNotInitialized reports use before a successful handshake. It carries the
// source_unavailable code, so errors.Is against the sentinel still matches,
// while keeping a message the wire can show.
var ErrNotInitialized = protocol.Errorf(protocol.CodeSourceUnavailable,
	"ongoing adapter has not completed a handshake")

// Settings is the adapter-owned settings block, declared by
// [recall.Manifest.SettingsSchema] and validated here on every handshake.
//
// There is deliberately no key for the access secret. ongoing takes it from the
// environment, a settings block travels inside a committed configuration file,
// and additionalProperties is false — so a configuration that tried to carry
// one fails the handshake with a readable error rather than putting a
// credential in a repository.
type Settings struct {
	// Views restricts this source to projects in any of ongoing's attention
	// classifications. It is what turns one catalog into several Recall
	// sources: "everything" and "the things that need attention" have
	// different priors and different answers, and they are the same API.
	Views []string `json:"views"`

	MaxCandidates int `json:"max_candidates"`

	// TimeoutMS bounds one API call. It composes with the request deadline;
	// the sooner of the two wins.
	TimeoutMS int `json:"timeout_ms"`

	// Replay names a directory of recorded API responses. When set, no request
	// leaves this process: the adapter under test is the parsing, the ranking,
	// and the freshness arithmetic, over a source that a conformance
	// transcript can hold still and a benchmark cannot race.
	Replay string `json:"replay"`

	// StallMS delays every search before it fetches. It exists so the
	// cancellation conformance case can be recorded deterministically against a
	// real process, and it is the shortest demonstration of the only thing an
	// adapter owes a cancel: notice the context, return, do not answer. Leave
	// it unset in real configuration.
	StallMS int `json:"debug_stall_ms"`
}

// Options are construction-time seams. Instance policy arrives at the
// handshake, because that is where the spec puts it.
//
// There is deliberately no seam for the transport. Everything a test would
// inject one for — a catalog that cannot change, an instance that refuses, an
// instance that is not there — the `replay` setting already expresses, over the
// same code path a conformance transcript and an evaluation pack use. A second
// way in would be a second thing to keep true.
type Options struct {
	// Clock supplies observation and probe timestamps. Nil means [time.Now].
	Clock func() time.Time
}

// Adapter is the ongoing project catalog source.
//
// The zero value is not usable; build one with [New]. It holds no projection:
// every search reads the catalog through the API, so there is nothing to
// publish, nothing to checkpoint, and nothing for [Adapter.Close] to release
// except the right to answer.
type Adapter struct {
	opts Options

	mu        sync.RWMutex
	ready     bool
	closed    bool
	sourceID  string
	set       Settings
	floor     recall.Sensitivity
	transport transport

	// secretSet records whether the environment offered an access secret. The
	// value is never stored here and never logged; only whether there was one,
	// so a denied probe can say which side of the boundary to look at.
	secretSet bool
}

// New returns an uninitialized adapter.
func New(opts Options) *Adapter { return &Adapter{opts: opts} }

// Initialize negotiates the protocol version, validates the settings block,
// resolves the ongoing instance, and reads the access secret from the
// environment. It contacts nothing: a handshake that probed the network would
// compete with the handshake timeout and would make a source that is merely
// asleep look like a source that is misconfigured.
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
	base, err := resolveLocation(cfg.Location)
	if err != nil {
		return recall.Manifest{}, err
	}

	// The environment is read only when there is a live instance to
	// authenticate against. A recording has no credentials to offer, and an
	// adapter whose replayed answers changed depending on whether a variable
	// happened to be exported would make a conformance transcript a recording
	// of the machine rather than of the adapter.
	var secret string
	if set.Replay == "" {
		secret = os.Getenv(SecretEnvVar)
	}

	tr, err := buildTransport(cfg, set, base, secret)
	if err != nil {
		return recall.Manifest{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.sourceID = cfg.SourceID
	a.set = set
	a.floor = recall.SensitivityInternal
	a.transport = tr
	a.secretSet = secret != ""
	a.ready = true

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,
		RecordTypes:     []recall.RecordType{RecordProject},
		QueryModes: []recall.QueryMode{
			recall.QueryExact, recall.QueryLexical, recall.QueryStructured, recall.QueryTemporal,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},

		// The catalog holds current state plus a daily snapshot of eight
		// numeric metrics. Note, intent, next action, attention membership,
		// LOC, and the td counts have no history at all, so state at a past
		// instant can be neither reconstructed nor filtered for. See the
		// package doc.
		AsOfSupport: recall.AsOfNone,

		// No checkpoint: this source owns no projection, so there is nothing
		// recall/refresh could bring up to date that a search does not already
		// read fresh.
		Capabilities: []recall.Capability{recall.CapSearch, recall.CapExpand},
		FreshnessPolicy: "live: every search reads the catalog through ongoing's API; the catalog " +
			"itself is rescanned daily at 04:00 and reports degraded once its last complete scan " +
			"is more than 72h behind, which is ongoing's own freshness rule",
		Sensitivity:    recall.SensitivityInternal,
		SettingsSchema: settingsSchema(),
	}, nil
}

// buildTransport chooses between the live instance and a recording.
func buildTransport(cfg adapter.Config, set Settings, base, secret string) (transport, error) {
	if set.Replay == "" {
		return newHTTPTransport(base, set.timeout(), secret), nil
	}
	// Relative to the directory of the file that declared the source. The
	// location names an HTTP endpoint, so unlike the Tasks adapter there is no
	// store directory a relative path could sensibly mean, and resolving
	// against the process working directory would make one configuration read
	// different recordings depending on where Recall was started.
	dir := set.Replay
	if !filepath.IsAbs(dir) {
		if cfg.BaseDir == "" {
			return nil, protocol.Errorf(protocol.CodeInvalidParams,
				"ongoing settings: replay %q is relative and this source was declared by no file, "+
					"so there is nothing to resolve it against", set.Replay)
		}
		dir = filepath.Join(cfg.BaseDir, dir)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing settings: replay %q is not a readable directory", set.Replay)
	}
	return replayTransport{dir: dir}, nil
}

// Close releases the adapter. Later calls must fail rather than answer for a
// source nobody is talking to any more.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.ready = false
	a.transport = nil
	return nil
}

// session snapshots what one request needs, or names the reason there is none.
// Holding the read lock for a whole search would serialize searches against a
// source that is perfectly happy to answer several at once.
func (a *Adapter) session() (Settings, transport, string, recall.Sensitivity, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.closed:
		return Settings{}, nil, "", 0, adapter.ErrClosed
	case !a.ready:
		return Settings{}, nil, "", 0, ErrNotInitialized
	}
	return a.set, a.transport, a.sourceID, a.floor, nil
}

func (a *Adapter) now() time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	return time.Now().UTC()
}

// fetch reads the catalog. Every search and every expansion goes through here:
// this source is live, so there is exactly one way to learn what it says.
func (a *Adapter) fetch(ctx context.Context, tr transport, set Settings) (*catalog, error) {
	ctx, cancel := withTimeout(ctx, set.timeout())
	defer cancel()

	body, err := tr.get(ctx, projectsPath)
	if err != nil {
		return nil, err
	}
	return parseCatalog(body)
}

// withTimeout bounds one API call without ever extending the caller's
// deadline. The request's own deadline is already on the context; this only
// ever tightens it.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// Health probes the instance and then the catalog.
//
// Both halves are asked because they answer different questions. /api/health is
// public — ongoing exempts it from authentication — so it says whether the
// process is up at all; /api/projects says whether this source may read the
// catalog and how old it is. Without the first, an instance that is running but
// refusing us would be indistinguishable from one that is down.
func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, tr, _, _, err := a.session()
	if err != nil {
		return adapter.Unhealthy(err), nil
	}
	checked := a.now()

	probeCtx, cancel := withTimeout(ctx, set.timeout())
	if _, err := tr.get(probeCtx, healthPath); err != nil {
		cancel()
		return a.unhealthy(err), nil
	}
	cancel()

	cat, err := a.fetch(ctx, tr, set)
	if err != nil {
		return a.unhealthy(err), nil
	}
	return a.healthOf(cat, tr, set, checked), nil
}

// unhealthy renders a failed probe, adding the one fact about this machine that
// a denial makes worth knowing.
//
// Whether a secret was configured here is not information about the source: it
// reveals no record and confirms no account. It is the difference between
// "export ONGOING_ACCESS_SECRET" and "the secret you exported is wrong", which
// is the whole of what an operator staring at a denied source needs.
func (a *Adapter) unhealthy(err error) recall.Health {
	h := adapter.Unhealthy(err)
	if h.Status == recall.HealthDenied {
		a.mu.RLock()
		set := a.secretSet
		a.mu.RUnlock()
		if h.Diagnostics == nil {
			h.Diagnostics = map[string]any{}
		}
		h.Diagnostics["access_secret_configured"] = set
	}
	return h
}

// healthOf reports on a catalog that was read successfully.
func (a *Adapter) healthOf(cat *catalog, tr transport, set Settings, checked time.Time) recall.Health {
	h := recall.Health{
		Status:          recall.HealthHealthy,
		CheckedAt:       checked,
		SourceWatermark: cat.watermark(),
		RecordCount:     int64(len(cat.Projects)),
		Coverage:        cat.coverage(),
		Diagnostics: map[string]any{
			"transport": tr.kind(),
			"projects":  len(cat.Projects),
			"views":     cat.viewCounts(),
		},
	}
	if finished, ok := cat.boundary(); ok {
		h.LastSuccess = &finished
	}
	if cat.HiddenCount > 0 {
		// Hidden projects are the owner's deliberate exclusion, not records
		// this source failed to read. Saying how many keeps the count visible
		// without pretending the boundary was incomplete.
		h.Diagnostics["hidden_projects"] = cat.HiddenCount
	}
	if n := cat.warnings(); n > 0 {
		h.Diagnostics["collector_warnings"] = n
	}
	if len(set.Views) > 0 {
		h.Diagnostics["views_filter"] = set.Views
	}

	switch {
	case cat.Scan == nil:
		// Nothing has ever scanned. Every project record is whatever was
		// written by hand, and no measurement has a boundary behind it.
		h.Status = recall.HealthDegraded
		h.Diagnostics["reason"] = "the catalog has never been scanned"
	case cat.Scan.Status != scanCompleted:
		h.Status = recall.HealthDegraded
		h.Diagnostics["reason"] = "the latest scan is " + cat.Scan.Status
		h.Diagnostics["scan_status"] = cat.Scan.Status
		if !cat.Scan.StartedAt.IsZero() {
			// A scan that started four minutes ago and one that has been
			// running since Tuesday are the same status and very different
			// problems.
			h.Diagnostics["scan_started_at"] = cat.Scan.StartedAt.UTC().Format(time.RFC3339)
		}
	case cat.stale():
		age, _ := cat.age()
		h.Status = recall.HealthDegraded
		h.Diagnostics["reason"] = "the catalog is older than ongoing's own freshness rule"
		h.Diagnostics["catalog_age_hours"] = int(age.Hours())
		h.Diagnostics["freshness_rule_hours"] = int(StaleAfter.Hours())
	case cat.collectorsDegraded():
		// A scan can finish minutes ago and still hold measurements days old:
		// ongoing ages each collector separately, and a classification whose
		// inputs are stale is not computed at all. Reporting healthy here would
		// turn that silence into "nothing qualifies" — the caller would read a
		// complete answer where the server simply declined to decide.
		h.Status = recall.HealthDegraded
		h.Diagnostics["reason"] = "the catalog is current but some collectors are not"
		h.Diagnostics["freshness_rule_hours"] = int(StaleAfter.Hours())
	}
	for k, v := range cat.staleCollectors() {
		// Reported whatever the status: a healthy catalog with two unmeasured
		// collectors is still a catalog whose classifications rest on less than
		// it looks like.
		h.Diagnostics[k] = v
	}
	if cat.Scan != nil && cat.Scan.ErrorCount > 0 {
		h.Diagnostics["scan_errors"] = cat.Scan.ErrorCount
	}
	return h
}

// Refresh reports current health without doing any work.
//
// This source is live and owns no projection, so there is nothing to bring up
// to date. The method exists because the contract has it, and returning health
// unchanged is the honest answer: reporting success for work never done would
// let a caller believe a stale catalog had been rescanned. Rescanning is
// ongoing's own daily LaunchAgent, and it takes five minutes — asking for it
// from inside a request deadline would be a promise this adapter cannot keep.
func (a *Adapter) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	return a.Health(ctx)
}

// resolveLocation reads the configured endpoint.
//
// It must be an absolute http or https URL with no path, query, or credentials.
// Settings and locations are adapter-owned and unvalidated when configuration
// is loaded, so this is the layer that has to refuse "file:///etc/passwd" and a
// URL with a password in it — nothing above it will.
func resolveLocation(location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: this source has no location, so there is no instance to ask")
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: location %q is not a URL", location)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: location %q must be an http or https URL", location)
	}
	if u.Host == "" {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: location %q names no host", location)
	}
	if u.User != nil {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: location must not carry credentials; the access secret is read from %s",
			SecretEnvVar)
	}
	if p := strings.Trim(u.Path, "/"); p != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing: location %q must be a bare origin; the endpoints are this adapter's to choose",
			location)
	}
	return u.Scheme + "://" + u.Host, nil
}

// parseSettings decodes and validates the settings block. Unknown keys are
// rejected: a misspelled setting that silently did nothing would be
// configuration with no code path behind it, which docs/spec.md calls a defect
// rather than a tolerance — and it is what keeps an access secret out of a
// committed file.
func parseSettings(raw map[string]any) (Settings, error) {
	var set Settings
	if len(raw) == 0 {
		return set, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "ongoing settings: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams, "ongoing settings: %v", err)
	}
	for _, view := range set.Views {
		if !knownView(view) {
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"ongoing settings: view %q is not one of %s", view, strings.Join(viewKeys, ", "))
		}
	}
	if set.MaxCandidates < 0 || set.TimeoutMS < 0 || set.StallMS < 0 {
		return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
			"ongoing settings: max_candidates, timeout_ms, and debug_stall_ms cannot be negative")
	}
	return set, nil
}

func (s Settings) maxCandidates() int {
	if s.MaxCandidates > 0 {
		return s.MaxCandidates
	}
	return DefaultMaxCandidates
}

func (s Settings) timeout() time.Duration {
	if s.TimeoutMS > 0 {
		return time.Duration(s.TimeoutMS) * time.Millisecond
	}
	return DefaultTimeout
}

// keeps reports whether a project passes the instance's configured view
// filter. No filter admits everything; several views are an OR, so adding one
// can only widen the set — the opposite reading would make "attention or
// momentum" mean "neither".
func (s Settings) keeps(p *project) bool {
	if len(s.Views) == 0 {
		return true
	}
	for _, want := range s.Views {
		if p.inView(want) {
			return true
		}
	}
	return false
}

// settingsSchema declares the settings block. `recall doctor` validates a
// configuration against it without starting a query, so every key here is one
// the code above reads.
func settingsSchema() map[string]any {
	views := make([]any, 0, len(viewKeys))
	for _, key := range viewKeys {
		views = append(views, key)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"views": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "enum": views},
				"description": "Restrict this source to projects in any of ongoing's attention " +
					"classifications. Empty means every visible project.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Cap on candidates returned by one search.",
			},
			"timeout_ms": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Per-request timeout. Composes with the request deadline; the sooner wins.",
			},
			"replay": map[string]any{
				"type": "string",
				"description": "Directory of recorded API responses named <endpoint>.<status>.json, " +
					"relative to the declaring configuration file. When set, no request leaves the " +
					"process, which is what lets a conformance transcript and an evaluation pack " +
					"exercise this adapter without a live instance.",
			},
			"debug_stall_ms": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Artificial pre-fetch delay, for recording the cancellation conformance case. Leave unset in real configuration.",
			},
		},
	}
}

// oneLine collapses a source-supplied string onto a single line.
//
// Everything rendered into evidence is data, never instructions, and the
// rendering here is line-oriented: a note reading "\n\nAttention:\n…" would
// otherwise forge a section header in text a model reads. Collapsing the
// whitespace is what keeps a field's value inside its field.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip bounds a string at a rune boundary, so a truncated preview is still
// text.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// stall delays a request and returns as soon as the context is done.
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

// number renders an optional integer for metadata, dropping the key entirely
// when the catalog has no value. A missing measurement is unknown, not zero,
// and writing a zero would let "never scanned" read as "no commits".
func number(into map[string]any, key string, value *int) {
	if value != nil {
		into[key] = *value
	}
}

func text(into map[string]any, key, value string) {
	if v := oneLine(value); v != "" {
		into[key] = v
	}
}

func stamp(into map[string]any, key string, value *time.Time) {
	if value != nil {
		into[key] = value.UTC().Format(time.RFC3339)
	}
}

// fmtValue renders an attention reason's value or threshold for text. The JSON
// side keeps the original typed value; this is only for the rendered lines.
func fmtValue(v any) string {
	switch typed := v.(type) {
	case nil:
		return "none"
	case string:
		return oneLine(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%g", typed)
	default:
		return oneLine(fmt.Sprint(typed))
	}
}

var _ adapter.Adapter = (*Adapter)(nil)
