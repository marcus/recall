package adapter_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// The full lifecycle against a real process: spawn, handshake with a writable
// workdir, search, expand, probe, clean exit.
func TestSubprocessLifecycle(t *testing.T) {
	ext := externalFixture(t, modeOK, adapter.Options{})
	ctx := context.Background()

	manifest, err := ext.Initialize(ctx, adapter.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != protocol.MaxVersion {
		t.Errorf("negotiated version %d", manifest.ProtocolVersion)
	}
	if manifest.AdapterID != "recall-fixture/0.1.0" {
		t.Errorf("adapter_id = %q", manifest.AdapterID)
	}

	resp, err := ext.Search(ctx, searchIn("recall protocol", 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess || len(resp.Candidates) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if got := resp.Candidates[0].Title; got != "recall protocol" {
		t.Errorf("title = %q, want the query echoed back", got)
	}

	ev, err := ext.Expand(ctx, recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "fixture", Local: "rec-1"},
		Detail:   recall.DetailFull,
		Budget:   4096,
		Deadline: time.Now().Add(10 * time.Second).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Content != "evidence for rec-1" || ev.Truncated {
		t.Fatalf("evidence = %+v", ev)
	}

	health, err := ext.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != recall.HealthHealthy {
		t.Errorf("status = %s", health.Status)
	}
	// Cold start is real work the request paid for, so it must be visible
	// rather than folded into total latency.
	if health.ColdStart <= 0 {
		t.Error("the first probe after a spawn must report cold start")
	}

	if err := ext.Close(); err != nil {
		t.Errorf("clean shutdown: %v", err)
	}
	if diag := ext.Diagnostics(); diag["spawns"] != 1 {
		t.Errorf("spawns = %v, want one process for one source instance", diag["spawns"])
	}
}

// The handshake promises a writable directory under Recall's state directory.
// An adapter's index has nowhere else legitimate to go.
func TestHandshakeSuppliesAWritableWorkdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "profile", "fixture")
	ext := adapter.NewExternal(adapter.Spec{
		Name:    "fixture",
		Command: os.Args[0],
		Env:     append(os.Environ(), fixtureModeEnv+"="+modeOK),
		Config: adapter.Config{
			ProtocolVersionMin: protocol.MinVersion,
			ProtocolVersionMax: protocol.MaxVersion,
			Workdir:            dir,
			SourceID:           "tasks",
		},
	})
	t.Cleanup(func() { _ = ext.Close() })

	if _, err := ext.Initialize(context.Background(), adapter.Config{}); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "index.marker"))
	if err != nil {
		t.Fatalf("adapter could not write to its workdir: %v", err)
	}
	if string(marker) != modeOK {
		t.Errorf("marker = %q", marker)
	}
}

// A version range with no overlap is a failure. Degrading to a version neither
// end implements would produce results nobody can reason about.
func TestUnsatisfiableVersionFailsTheHandshake(t *testing.T) {
	ext := externalFixture(t, modeBadVersion, adapter.Options{})

	_, err := ext.Initialize(context.Background(), adapter.Config{})
	var version *protocol.VersionError
	if !errors.As(err, &version) {
		t.Fatalf("err = %v, want a VersionError", err)
	}
	if version.Offered != 99 {
		t.Errorf("offered = %d", version.Offered)
	}

	// And the failure must reach a query as a failure, not as no results.
	resp, err := ext.Search(context.Background(), searchIn("q", 5*time.Second))
	if err == nil {
		t.Fatal("search on an unusable adapter must fail")
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Fatalf("outcome = %s", resp.Outcome)
	}
}

// stderr is free-form logging. A frame written there is still just a log line:
// nothing on that channel can answer a request.
func TestStderrBecomesDiagnosticsAndNothingElse(t *testing.T) {
	ext := externalFixture(t, modeStderr, adapter.Options{})
	ctx := context.Background()

	resp, err := ext.Search(ctx, searchIn("hello", 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// The forged frame on stderr claimed an empty success for request id 1.
	// The real answer is a candidate.
	if len(resp.Candidates) != 1 {
		t.Fatalf("stderr answered the request: %+v", resp)
	}

	diag := ext.Diagnostics()
	lines, _ := diag["stderr"].([]string)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "opening the source") {
		t.Fatalf("stderr was not captured: %v", diag)
	}
	if !strings.Contains(joined, `"jsonrpc"`) {
		t.Fatalf("the frame on stderr should be kept verbatim as a log line: %v", diag)
	}
	if diag["protocol_violations"] != nil {
		t.Errorf("stderr must not be counted as a protocol violation: %v", diag)
	}
}

// stdout carries frames only. A stray line is a contract break, recorded and
// skipped — the stream stays framed and the request still succeeds.
func TestNoiseOnStdoutIsRecordedNotFatal(t *testing.T) {
	ext := externalFixture(t, modeNoise, adapter.Options{})

	resp, err := ext.Search(context.Background(), searchIn("q", 10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if v := ext.Diagnostics()["protocol_violations"]; v == nil {
		t.Error("a non-frame line on stdout should be recorded as a violation")
	}
}

// The deadline path end to end: cancel is ignored, so the process is signalled,
// and the request is a timeout rather than an empty answer.
func TestHangingAdapterIsTerminatedAndReportedAsTimeout(t *testing.T) {
	ext := externalFixture(t, modeHang, adapter.Options{
		CancelGrace: 100 * time.Millisecond,
		TermGrace:   time.Second,
	})

	resp, err := ext.Search(context.Background(), searchIn("slow", 200*time.Millisecond))
	if err == nil {
		t.Fatal("a request past its deadline must fail")
	}
	if resp.Outcome != recall.SearchTimeout {
		t.Fatalf("outcome = %s, want timeout", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Fatal("a timeout must not carry candidates")
	}

	// The fixture ignores both the cancel notification and its stdin closing,
	// so nothing but the signal could have ended it.
	if exit := waitForExit(t, ext); !strings.Contains(exit, "terminated") {
		t.Fatalf("last exit = %q, want SIGTERM", exit)
	}
}

// An adapter that ignores SIGTERM is killed. Without the second step a wedged
// process would outlive the query that spawned it.
func TestAdapterIgnoringSigtermIsKilled(t *testing.T) {
	ext := externalFixture(t, modeWedge, adapter.Options{
		CancelGrace: 100 * time.Millisecond,
		TermGrace:   500 * time.Millisecond,
	})

	resp, err := ext.Search(context.Background(), searchIn("wedged", 200*time.Millisecond))
	if err == nil {
		t.Fatal("a request past its deadline must fail")
	}
	if resp.Outcome != recall.SearchTimeout {
		t.Fatalf("outcome = %s, want timeout", resp.Outcome)
	}

	exit := waitForExit(t, ext)
	if !strings.Contains(exit, "killed") {
		t.Fatalf("last exit = %q, want a SIGKILL after SIGTERM was ignored", exit)
	}
}

// A killed adapter is respawned on the next use, not retried inside the request
// that killed it. The second query gets a fresh process and a real answer.
func TestKilledAdapterRespawnsOnNextUse(t *testing.T) {
	ext := externalFixture(t, modeHangFirst, adapter.Options{
		CancelGrace: 100 * time.Millisecond,
		TermGrace:   time.Second,
	})

	if _, err := ext.Search(context.Background(), searchIn("first", 200*time.Millisecond)); err == nil {
		t.Fatal("the first query should have timed out")
	}
	waitForExit(t, ext)

	resp, err := ext.Search(context.Background(), searchIn("second", 10*time.Second))
	if err != nil {
		t.Fatalf("the respawned adapter should answer: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	if spawns := ext.Diagnostics()["spawns"]; spawns != 2 {
		t.Errorf("spawns = %v, want a second process", spawns)
	}
}

// Cold start counts against the request budget that paid it, and only that one.
// A warm probe reporting cold start would double-count it in every report.
func TestColdStartIsReportedOnceAfterSpawn(t *testing.T) {
	ext := externalFixture(t, modeOK, adapter.Options{HealthTTL: 20 * time.Millisecond})
	ctx := context.Background()

	first, err := ext.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.ColdStart <= 0 {
		t.Fatal("the first probe after a spawn must report cold start")
	}

	time.Sleep(40 * time.Millisecond)
	second, err := ext.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.ColdStart != 0 {
		t.Errorf("a warm probe reported cold start %v", second.ColdStart)
	}
}

func waitForExit(t *testing.T, ext *adapter.External) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exit, ok := ext.Diagnostics()["last_exit"].(string); ok && exit != "" {
			return exit
		}
		time.Sleep(5 * time.Millisecond)
	}
	return ""
}

const forkerMarkerEnv = "RECALL_TEST_FORKER_MARKER"

// A budget is a promise to the caller. Tearing the process down used to run
// inside the request that timed out, so the whole grace ladder — cancel grace,
// SIGTERM grace, then the wait after SIGKILL — was spent on the caller's clock
// and a 200ms budget returned seconds later.
func TestTimedOutRequestReturnsWithinItsBudget(t *testing.T) {
	ext := externalFixture(t, modeWedge, adapter.Options{
		CancelGrace: 100 * time.Millisecond,
		TermGrace:   3 * time.Second,
	})

	start := time.Now()
	resp, err := ext.Search(context.Background(), searchIn("q", 200*time.Millisecond))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a wedged adapter must not report success")
	}
	if resp.Outcome.Searched() {
		t.Errorf("outcome = %s, want a failure", resp.Outcome)
	}
	// Budget plus cancel grace plus slack. Nowhere near the termination ladder.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("returned after %v; teardown must not run on the caller's clock", elapsed)
	}
}

// Signalling only the direct child leaves grandchildren running with the
// descriptors they inherited, so "the process is gone when Close returns" was
// false for anything an adapter forks.
func TestTerminationReachesGrandchildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive")
	ext := adapter.NewExternal(adapter.Spec{
		Name:    "forker",
		Command: os.Args[0],
		Env: append(os.Environ(),
			fixtureModeEnv+"="+modeForker,
			forkerMarkerEnv+"="+marker),
		Config: adapter.Config{
			ProtocolVersionMin: protocol.MinVersion,
			ProtocolVersionMax: protocol.MaxVersion,
			Workdir:            filepath.Join(t.TempDir(), "work"),
			SourceID:           "tasks",
		},
		Options: adapter.Options{CancelGrace: 100 * time.Millisecond, TermGrace: time.Second},
	})

	if _, err := ext.Health(context.Background()); err != nil {
		t.Skipf("fixture did not start: %v", err)
	}

	size := func() int64 {
		fi, err := os.Stat(marker)
		if err != nil {
			return -1
		}
		return fi.Size()
	}
	// Wait for the grandchild to prove it is running, so the assertion below is
	// about termination rather than about startup timing.
	deadline := time.Now().Add(2 * time.Second)
	for size() <= 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if size() <= 0 {
		t.Skip("grandchild never started; nothing to prove")
	}

	if err := ext.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Logf("close: %v", err)
	}
	before := size()
	time.Sleep(400 * time.Millisecond)
	if after := size(); after != before {
		t.Errorf("grandchild still writing after Close (%d -> %d bytes)", before, after)
	}
}
