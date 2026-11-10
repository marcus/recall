package qmd_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// conflictRunner answers like healthyRunner, but a maintenance invocation that
// overlaps another fails the way two qmd writers on one SQLite index do (#16).
func conflictRunner(root string, hold func(args []string)) (*fakeRunner, *atomic.Int32) {
	r := healthyRunner(root, "[]")
	delete(r.byKey, "update")
	delete(r.byKey, "embed")
	var inFlight, peak atomic.Int32
	r.fallback = func(args []string) (qmd.Result, error) {
		if args[0] != "update" && args[0] != "embed" {
			return qmd.Result{ExitCode: 1, Stderr: []byte("unrecorded")}, nil
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if hold != nil {
			hold(args)
		}
		if n > 1 {
			return qmd.Result{ExitCode: 1, Stderr: []byte("SQLiteError: ON CONFLICT(collection, path) DO UPDATE SET")}, nil
		}
		return qmd.Result{Stdout: []byte("✓ All collections updated.\n")}, nil
	}
	return r, &peak
}

func TestRefreshSerializesMaintenanceOnOneIndex(t *testing.T) {
	root := corpus(t)
	runner, peak := conflictRunner(root, func([]string) { time.Sleep(30 * time.Millisecond) })
	a := newAdapter(t, root, baseSettings(), runner)
	b := newAdapter(t, root, baseSettings(), runner)

	var wg sync.WaitGroup
	results := make([]recall.Health, 2)
	errs := make([]error, 2)
	for i, src := range []*qmd.Adapter{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = src.Refresh(t.Context(), protocol.RefreshParams{})
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("refresh %d: %v", i, errs[i])
		}
		if results[i].Status != recall.HealthHealthy {
			t.Errorf("refresh %d status = %q: %v", i, results[i].Status, results[i].Diagnostics)
		}
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("maintenance overlapped: %d qmd writers at once", got)
	}
}

func TestRefreshWaitingOnIndexLockHonorsDeadline(t *testing.T) {
	root := corpus(t)
	entered := make(chan struct{})
	gate := make(chan struct{})
	var first atomic.Bool
	var opened sync.Once
	openGate := func() { opened.Do(func() { close(gate) }) }
	runner, _ := conflictRunner(root, func(args []string) {
		// Only the first update parks; sync.Once would park later callers too.
		if args[0] == "update" && first.CompareAndSwap(false, true) {
			close(entered)
			<-gate
		}
	})
	a := newAdapter(t, root, baseSettings(), runner)
	b := newAdapter(t, root, baseSettings(), runner)
	// Registered after the adapters so it runs before they close: a failed
	// assertion must not strand the lock holder inside Close.
	t.Cleanup(openGate)

	done := make(chan error, 1)
	go func() {
		_, err := a.Refresh(t.Context(), protocol.RefreshParams{})
		done <- err
	}()
	<-entered
	updatesBefore := countRuns(runner, "update")
	_, err := b.Refresh(t.Context(), protocol.RefreshParams{Deadline: time.Now().Add(80 * time.Millisecond)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting refresh = %v, want deadline exceeded", err)
	}
	if got := countRuns(runner, "update"); got != updatesBefore {
		t.Fatalf("a refresh ran update without the index lock")
	}
	openGate()
	if err := <-done; err != nil {
		t.Fatalf("lock holder: %v", err)
	}
}

// A source that cannot learn its index path must not fall back to a lock no
// sibling on the real index would meet; it refuses maintenance instead (#16).
func TestRefreshRefusesMaintenanceWithoutIndexPath(t *testing.T) {
	root := corpus(t)
	runner := healthyRunner(root, "[]")
	var kept []string
	for line := range strings.SplitSeq(statusText(root, "fixture", 2, 2, 2), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "Index:") {
			kept = append(kept, line)
		}
	}
	runner.byKey["status"] = qmd.Result{Stdout: []byte(strings.Join(kept, "\n"))}
	a := newAdapter(t, root, baseSettings(), runner)
	h, err := a.Refresh(t.Context(), protocol.RefreshParams{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if h.Status != recall.HealthUnavailable {
		t.Errorf("status = %q, want unavailable: %v", h.Status, h.Diagnostics)
	}
	if n := countRuns(runner, "update") + countRuns(runner, "embed"); n != 0 {
		t.Fatalf("ran %d maintenance commands without the index lock", n)
	}
}

func countRuns(r *fakeRunner, sub string) int {
	n := 0
	for _, args := range r.invocations() {
		if len(args) > 0 && args[0] == sub {
			n++
		}
	}
	return n
}
