package adapter_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// The fixture adapter is this test binary, re-executed.
//
// Running a real process is the only way to exercise spawn, stdin/stdout
// framing, stderr capture, SIGTERM, and SIGKILL. Re-executing the test binary
// keeps that hermetic: no build step, no separate module, and the fixture is
// the same [adapter.Adapter] the in-process tests use, served through
// [adapter.Serve] — which is what makes "one contract, two transports" a claim
// the tests can check rather than an assertion in a comment.
const fixtureModeEnv = "RECALL_TEST_ADAPTER_MODE"

// Fixture behaviors. Each one models a way a real adapter misbehaves.
const (
	modeOK = "ok"
	// modeStderr writes to stderr, including a perfectly formed protocol
	// frame, which must never be treated as an answer.
	modeStderr = "stderr"
	// modeHang never answers a search and ignores the cancel notification.
	// SIGTERM ends it.
	modeHang = "hang"
	// modeWedge also ignores SIGTERM, so only SIGKILL ends it.
	modeWedge = "wedge"
	// modeHangFirst hangs once, then answers normally after the respawn. It
	// remembers across processes through a marker in the workdir.
	modeHangFirst = "hang-first"
	// modeCrash exits before the handshake.
	modeCrash = "crash"
	// modeBadVersion names a protocol version nobody asked for.
	modeBadVersion = "bad-version"
	// modeDenied refuses every search.
	modeDenied = "denied"
	// modeSerial declares max_concurrency: 1 and reports what it observed.
	modeSerial = "serial"
	// modeNoise writes junk on stdout before its frames.
	modeNoise = "noise"
	// modeForker spawns a child that outlives it, then wedges. Killing only
	// the direct process would leave the child running.
	modeForker = "forker"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fixtureModeEnv); mode != "" {
		os.Exit(runFixture(mode))
	}
	os.Exit(m.Run())
}

func runFixture(mode string) int {
	switch mode {
	case modeCrash:
		return 3
	case modeWedge:
		// Only SIGKILL can end this one, which is what the escalation exists
		// for.
		signal.Ignore(syscall.SIGTERM)
	case modeStderr:
		fmt.Fprintln(os.Stderr, "fixture: opening the source")
		fmt.Fprintln(os.Stderr, `{"jsonrpc":"2.0","id":1,"result":{"candidates":[],"outcome":"success"}}`)
	case modeNoise:
		fmt.Fprintln(os.Stdout, "this line is not a frame")
	case modeForker:
		signal.Ignore(syscall.SIGTERM)
		if path := os.Getenv(forkerMarkerEnv); path != "" {
			child := exec.Command("/bin/sh", "-c",
				"while :; do echo alive >> "+path+"; sleep 0.05; done")
			_ = child.Start()
		}
	}
	err := adapter.Serve(context.Background(), os.Stdin, os.Stdout, newFixture(mode))
	if hung.Load() {
		// A process that wedged on a request must not quietly exit when its
		// stdin closes, or the SIGTERM and SIGKILL steps would never run and
		// the escalation would be untested. Sleeping rather than blocking on a
		// channel keeps Go's deadlock detector from ending the process for us.
		for {
			time.Sleep(time.Hour)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		return 1
	}
	return 0
}

// hung records that this process abandoned a request. It is process-global
// because the escalation is about the process, not the request.
var hung atomic.Bool

// fixture is a built-in adapter that behaves badly on request.
type fixture struct {
	mode string

	mu       sync.Mutex
	workdir  string
	inFlight int
	peak     int
	probes   int

	never chan struct{}
}

func newFixture(mode string) *fixture {
	return &fixture{mode: mode, never: make(chan struct{})}
}

func (f *fixture) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version := 99
	if f.mode != modeBadVersion {
		negotiated, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
		if err != nil {
			return recall.Manifest{}, err
		}
		version = negotiated
	}

	if cfg.Workdir != "" {
		// The handshake promises a writable directory. Writing to it is how
		// the test knows the promise was kept.
		if err := os.WriteFile(filepath.Join(cfg.Workdir, "index.marker"), []byte(f.mode), 0o600); err != nil {
			return recall.Manifest{}, err
		}
	}
	f.mu.Lock()
	f.workdir = cfg.Workdir
	f.mu.Unlock()

	concurrency := 0
	if f.mode == modeSerial {
		concurrency = 1
	}
	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       "recall-fixture/0.1.0",
		DisplayName:     "Fixture",
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		QueryModes:      []recall.QueryMode{recall.QueryLexical},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessLive},
		AsOfSupport:     recall.AsOfNone,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		MaxConcurrency:  concurrency,
		FreshnessPolicy: "read-through, no index",
		Sensitivity:     recall.SensitivityInternal,
	}, nil
}

func (f *fixture) enter() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
	return f.peak
}

func (f *fixture) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}

func (f *fixture) Search(_ context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	peak := f.enter()
	defer f.leave()

	switch f.mode {
	case modeDenied:
		return recall.SearchResponse{}, protocol.Errorf(protocol.CodeSourceDenied, "not permitted")
	case modeHang, modeWedge:
		hung.Store(true)
		<-f.never
	case modeHangFirst:
		f.mu.Lock()
		marker := filepath.Join(f.workdir, "hung.marker")
		f.mu.Unlock()
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, []byte("1"), 0o600)
			hung.Store(true)
			<-f.never
		}
	case modeSerial:
		// Long enough that a broken semaphore would overlap requests.
		time.Sleep(20 * time.Millisecond)
	}

	return recall.SearchResponse{
		Candidates: []recall.Candidate{{
			CandidateID:    "c-1",
			SourceRecordID: "rec-1",
			// The prefix is the adapter's own name; the core replaces it with
			// the configured source_id when it attaches identity.
			Locator:      recall.Locator{SourceID: "fixture", Local: "rec-1"},
			RecordType:   recall.RecordDocument,
			Title:        req.Query,
			Excerpt:      "matched " + req.Query,
			LocalRank:    1,
			MatchSignals: []recall.MatchSignal{recall.MatchLexical},
			Sensitivity:  recall.SensitivityInternal,
		}},
		SourceWatermark: "rev-1",
		Outcome:         recall.SearchSuccess,
		Diagnostics:     map[string]any{"peak_in_flight": peak},
	}, nil
}

func (f *fixture) Expand(_ context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	if req.Locator.Local == "gone" {
		return recall.ExpandResponse{}, protocol.Errorf(protocol.CodeLocatorExpired, "revision moved")
	}
	body := "evidence for " + req.Locator.Local
	truncated := false
	boundary := ""
	if req.Budget > 0 && int64(len(body)) > req.Budget {
		body = body[:req.Budget]
		truncated = true
		boundary = "budget_bytes"
	}
	return recall.ExpandResponse{
		Content:            body,
		SourceRevision:     "rev-1",
		Truncated:          truncated,
		TruncationBoundary: boundary,
		Provenance:         req.Locator.Local,
	}, nil
}

func (f *fixture) Health(context.Context) (recall.Health, error) {
	f.mu.Lock()
	f.probes++
	probes := f.probes
	f.mu.Unlock()

	return recall.Health{
		Status:      recall.HealthHealthy,
		CheckedAt:   time.Now().UTC(),
		Coverage:    recall.IndexComplete,
		RecordCount: 1,
		Diagnostics: map[string]any{"probes": probes},
	}, nil
}

func (f *fixture) Close() error { return nil }

func (f *fixture) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes
}

var _ adapter.Adapter = (*fixture)(nil)

// externalFixture builds an External that re-executes this test binary in the
// given mode.
func externalFixture(t *testing.T, mode string, opt adapter.Options) *adapter.External {
	t.Helper()
	if _, err := exec.LookPath(os.Args[0]); err != nil && !filepath.IsAbs(os.Args[0]) {
		t.Skipf("test binary is not executable by path: %v", err)
	}
	ext := adapter.NewExternal(adapter.Spec{
		Name:    "fixture",
		Command: os.Args[0],
		Env:     append(os.Environ(), fixtureModeEnv+"="+mode),
		Config: adapter.Config{
			ProtocolVersionMin: protocol.MinVersion,
			ProtocolVersionMax: protocol.MaxVersion,
			Workdir:            filepath.Join(t.TempDir(), "work"),
			SourceID:           "tasks",
		},
		Options: opt,
	})
	t.Cleanup(func() { _ = ext.Close() })
	return ext
}

func searchIn(query string, d time.Duration) recall.SearchRequest {
	return recall.SearchRequest{
		Query:    query,
		Limit:    5,
		Deadline: time.Now().Add(d).UTC(),
	}
}
