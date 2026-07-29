package gmail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

const (
	AdapterID   = "recall-gmail/1"
	DisplayName = "Gmail"

	defaultScope = "-in:spam -in:trash -in:chats " +
		"-category:promotions -category:social -category:forums"
	defaultBrowse        = "in:inbox is:unread category:primary -from:me newer_than:14d"
	defaultTimeout       = 15 * time.Second
	defaultMaxCandidates = 25
	excerptBytes         = 240
)

var accountPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)

type Settings struct {
	GogBinary     string
	Timeout       time.Duration
	MaxCandidates int
	ScopeQuery    string
	BrowseQuery   string
	NewerThanDays int
	Replay        string
	DebugStall    time.Duration
}

type Options struct {
	Runner Runner
	Clock  func() time.Time
}

type Adapter struct {
	opts Options

	mu          sync.RWMutex
	ready       bool
	closed      bool
	sourceID    string
	account     string
	settings    Settings
	runner      Runner
	lastSuccess *time.Time
}

func New(opts Options) *Adapter { return &Adapter{opts: opts} }

var ErrNotInitialized = protocol.Errorf(protocol.CodeSourceUnavailable,
	"gmail adapter has not completed a handshake")

func (a *Adapter) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	account := strings.TrimSpace(cfg.Location)
	account = strings.TrimPrefix(account, "google://")
	if !accountPattern.MatchString(account) {
		return recall.Manifest{}, protocol.Errorf(protocol.CodeInvalidParams,
			"gmail: location must be the gog account address; configure location_kind = \"opaque\"")
	}
	set, err := parseSettings(cfg.Settings, cfg.BaseDir)
	if err != nil {
		return recall.Manifest{}, err
	}

	runner := a.opts.Runner
	if runner == nil {
		if set.Replay != "" {
			runner, err = newReplayRunner(set.Replay)
			if err != nil {
				return recall.Manifest{}, err
			}
		} else {
			runner = &liveRunner{
				binary:  set.GogBinary,
				account: account,
				timeout: set.Timeout,
			}
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return recall.Manifest{}, adapter.ErrClosed
	}
	a.ready = true
	a.sourceID = cfg.SourceID
	a.account = account
	a.settings = set
	a.runner = runner
	a.lastSuccess = nil

	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       AdapterID,
		DisplayName:     DisplayName,
		RecordTypes:     []recall.RecordType{recall.RecordMessage},
		QueryModes: []recall.QueryMode{
			recall.QueryExact,
			recall.QueryLexical,
			recall.QueryStructured,
			recall.QueryTemporal,
		},
		FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},
		AsOfSupport:    recall.AsOfNone,
		Capabilities:   []recall.Capability{recall.CapSearch, recall.CapExpand},
		MaxConcurrency: 2,
		FreshnessPolicy: "live: each search reads Gmail through gog; scope_query defines the " +
			"mail corpus, while browse, recency, and pagination boundaries report partial coverage",
		Sensitivity:    recall.SensitivityConfidential,
		SettingsSchema: settingsSchema(),
	}, nil
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
	return a.settings, a.sourceID, a.account, a.runner, nil
}

func (a *Adapter) now(r Runner) time.Time {
	if a.opts.Clock != nil {
		return a.opts.Clock().UTC()
	}
	if got, ok := r.Now(); ok {
		return got.UTC()
	}
	return time.Now().UTC()
}

func (a *Adapter) noteSuccess(at time.Time) {
	a.mu.Lock()
	at = at.UTC()
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

func (a *Adapter) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	return a.Health(ctx)
}

func (a *Adapter) Health(ctx context.Context) (recall.Health, error) {
	set, _, account, runner, err := a.session()
	now := time.Now().UTC()
	if runner != nil {
		now = a.now(runner)
	}
	if err != nil {
		return unhealthy(now, err), nil
	}

	var accounts authList
	if err := runner.Run(ctx, "auth-list", []string{"auth", "list"}, "", &accounts); err != nil {
		return unhealthy(now, err), nil
	}
	if len(accounts.Accounts) > 0 {
		found := false
		for _, got := range accounts.Accounts {
			if strings.EqualFold(got.Email, account) {
				found = true
				break
			}
		}
		if !found {
			return recall.Health{
				Status:    recall.HealthDenied,
				CheckedAt: now,
				Coverage:  recall.IndexUnknown,
				Diagnostics: map[string]any{
					"reason": "gog holds no credentials for the configured account",
				},
			}, nil
		}
	}

	scope := sanitizeLine(set.ScopeQuery)
	if scope == "" {
		scope = "in:inbox"
	}
	var probe searchPayload
	if err := runner.Run(ctx, "gmail-probe",
		[]string{"gmail", "search", "--max", "1", "-z", "UTC"}, scope, &probe); err != nil {
		return unhealthy(now, err), nil
	}
	a.noteSuccess(now)

	return recall.Health{
		Status:          recall.HealthHealthy,
		CheckedAt:       now,
		LastSuccess:     a.lastSuccessful(),
		SourceWatermark: watermark(scope, probe.Threads, probe.NextPageToken != ""),
		Coverage:        recall.IndexComplete,
		Diagnostics: map[string]any{
			"transport":    runner.Kind(),
			"scope_query":  scope,
			"browse_query": sanitizeLine(set.BrowseQuery),
		},
	}, nil
}

func unhealthy(now time.Time, err error) recall.Health {
	status := recall.HealthUnavailable
	if isDenied(err) {
		status = recall.HealthDenied
	}
	_, reason := adapter.Classify(err)
	return recall.Health{
		Status:      status,
		CheckedAt:   now,
		Coverage:    recall.IndexUnknown,
		Diagnostics: map[string]any{"reason": reason},
	}
}

func parseSettings(raw map[string]any, baseDir string) (Settings, error) {
	set := Settings{
		GogBinary:     "gog",
		Timeout:       defaultTimeout,
		MaxCandidates: defaultMaxCandidates,
		ScopeQuery:    defaultScope,
		BrowseQuery:   defaultBrowse,
	}
	allowed := map[string]bool{
		"gog_binary": true, "timeout_ms": true, "max_candidates": true,
		"scope_query": true, "browse_query": true, "newer_than_days": true,
		"replay": true, "debug_stall_ms": true,
	}
	for key := range raw {
		if !allowed[key] {
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"gmail: unknown setting %q", key)
		}
	}

	var err error
	if set.GogBinary, err = stringSetting(raw, "gog_binary", set.GogBinary); err != nil {
		return Settings{}, err
	}
	timeoutMS, err := intSetting(raw, "timeout_ms", int(defaultTimeout/time.Millisecond), 1)
	if err != nil {
		return Settings{}, err
	}
	set.Timeout = time.Duration(timeoutMS) * time.Millisecond
	if set.MaxCandidates, err = intSetting(raw, "max_candidates", set.MaxCandidates, 1); err != nil {
		return Settings{}, err
	}
	if set.ScopeQuery, err = stringSetting(raw, "scope_query", set.ScopeQuery); err != nil {
		return Settings{}, err
	}
	if set.BrowseQuery, err = stringSetting(raw, "browse_query", set.BrowseQuery); err != nil {
		return Settings{}, err
	}
	if set.NewerThanDays, err = intSetting(raw, "newer_than_days", 0, 0); err != nil {
		return Settings{}, err
	}
	if set.Replay, err = stringSetting(raw, "replay", ""); err != nil {
		return Settings{}, err
	}
	stallMS, err := intSetting(raw, "debug_stall_ms", 0, 0)
	if err != nil {
		return Settings{}, err
	}
	set.DebugStall = time.Duration(stallMS) * time.Millisecond

	set.GogBinary = strings.TrimSpace(set.GogBinary)
	if set.GogBinary == "" {
		set.GogBinary = "gog"
	}
	if set.Replay != "" && !filepath.IsAbs(set.Replay) {
		if baseDir == "" {
			return Settings{}, protocol.Errorf(protocol.CodeInvalidParams,
				"gmail: relative replay path requires base_dir")
		}
		set.Replay = filepath.Join(baseDir, set.Replay)
	}
	return set, nil
}

func stringSetting(raw map[string]any, key, fallback string) (string, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	got, ok := value.(string)
	if !ok {
		return "", protocol.Errorf(protocol.CodeInvalidParams,
			"gmail: setting %q must be a string", key)
	}
	return got, nil
}

func intSetting(raw map[string]any, key string, fallback, minimum int) (int, error) {
	value, ok := raw[key]
	if !ok {
		return fallback, nil
	}
	var got int
	switch n := value.(type) {
	case int:
		got = n
	case int64:
		got = int(n)
	case float64:
		if n != float64(int(n)) {
			return 0, protocol.Errorf(protocol.CodeInvalidParams,
				"gmail: setting %q must be an integer", key)
		}
		got = int(n)
	default:
		return 0, protocol.Errorf(protocol.CodeInvalidParams,
			"gmail: setting %q must be an integer", key)
	}
	if got < minimum {
		return 0, protocol.Errorf(protocol.CodeInvalidParams,
			"gmail: setting %q must be at least %d", key, minimum)
	}
	return got, nil
}

func settingsSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"gog_binary": map[string]any{
				"type": "string", "description": "Path to gog. Empty means gog on PATH.",
			},
			"timeout_ms": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Maximum duration of one gog invocation; the request deadline may shorten it.",
			},
			"max_candidates": map[string]any{
				"type": "integer", "minimum": 1,
				"description": "Maximum candidates returned by one Gmail search.",
			},
			"scope_query": map[string]any{
				"type":        "string",
				"description": "Gmail query fragment ANDed into every search; this defines the source corpus.",
			},
			"browse_query": map[string]any{
				"type":        "string",
				"description": "Gmail query used when Recall supplies an empty query.",
			},
			"newer_than_days": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Optional recency boundary added to every search; zero is unbounded.",
			},
			"replay": map[string]any{
				"type":        "string",
				"description": "Recorded gog fixture directory used by conformance tests; relative paths resolve from base_dir.",
			},
			"debug_stall_ms": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "Artificial pre-search delay for cancellation conformance tests.",
			},
		},
	}
}

func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:16])
}

func stall(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sourceRevision(count int, last any) string {
	return fmt.Sprintf("messages=%d last=%v", count, last)
}
