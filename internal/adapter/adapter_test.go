package adapter_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// connectFixture serves an in-process adapter over a pipe and returns the
// client side. It is the wire transport without a process, which is what makes
// the two-transport claim testable in one goroutine.
func connectFixture(t *testing.T, f adapter.Adapter, opt adapter.Options) *adapter.Conn {
	t.Helper()

	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = adapter.Serve(ctx, serverReads, serverWrites, f)
		_ = serverWrites.Close()
	}()

	conn, err := adapter.Connect(context.Background(), clientReads, clientWrites, adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
	}, opt)
	if err != nil {
		cancel()
		_ = serverReads.Close()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		cancel()
		_ = serverReads.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("the served adapter did not stop")
		}
	})
	return conn
}

func TestUnsupportedFiltersNamesOnlyPresentConstraints(t *testing.T) {
	resp, skipped := adapter.UnsupportedFilters(recall.Filters{
		Project: "recall", Entities: []string{"Marcus"},
	}, "entities", "project")
	if !skipped || resp.Outcome != recall.SearchSkipped || resp.Reason != recall.SkipFilterUnsupported {
		t.Fatalf("response = %+v, skipped = %v", resp, skipped)
	}
	if len(resp.Candidates) != 0 {
		t.Fatalf("candidates = %d, want none", len(resp.Candidates))
	}
	got, _ := resp.Diagnostics["unsupported_filters"].([]string)
	if len(got) != 2 || got[0] != "entities" || got[1] != "project" {
		t.Fatalf("unsupported_filters = %v", got)
	}
	if _, skipped := adapter.UnsupportedFilters(recall.Filters{}, "entities", "project"); skipped {
		t.Fatal("empty filters were skipped")
	}
}

// One contract, two transports. The same adapter answered directly and over the
// wire must produce the same candidates; a difference here means the JSON-RPC
// path has become a second contract.
func TestOneContractTwoTransports(t *testing.T) {
	direct := newFixture(modeOK)
	overWire := connectFixture(t, newFixture(modeOK), adapter.Options{})

	ctx := context.Background()
	req := searchIn("shared query", 10*time.Second)

	fromDirect, err := direct.Search(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	fromWire, err := overWire.Search(ctx, req)
	if err != nil {
		t.Fatal(err)
	}

	if fromDirect.Outcome != fromWire.Outcome {
		t.Errorf("outcome: direct %s, wire %s", fromDirect.Outcome, fromWire.Outcome)
	}
	if fromDirect.SourceWatermark != fromWire.SourceWatermark {
		t.Errorf("watermark: direct %q, wire %q", fromDirect.SourceWatermark, fromWire.SourceWatermark)
	}
	if !reflect.DeepEqual(fromDirect.Candidates, fromWire.Candidates) {
		t.Errorf("candidates differ between transports:\ndirect %+v\nwire   %+v",
			fromDirect.Candidates, fromWire.Candidates)
	}

	// Expansion too: a locator printed by one transport expands the same on the
	// other.
	loc := recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "fixture", Local: "rec-1"},
		Detail:   recall.DetailFull,
		Budget:   1024,
		Deadline: time.Now().Add(10 * time.Second).UTC(),
	}
	directEv, err := direct.Expand(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	wireEv, err := overWire.Expand(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(directEv, wireEv) {
		t.Errorf("evidence differs between transports:\ndirect %+v\nwire   %+v", directEv, wireEv)
	}
}

// An adapter that declares max_concurrency: 1 must never see two requests at
// once. The declaration is the adapter's only way to say it is not reentrant.
func TestMaxConcurrencyIsHonored(t *testing.T) {
	f := newFixture(modeSerial)
	conn := connectFixture(t, f, adapter.Options{})

	if got := conn.Manifest().MaxConcurrency; got != 1 {
		t.Fatalf("max_concurrency = %d, want 1", got)
	}

	var wg sync.WaitGroup
	peaks := make([]float64, 6)
	for i := range peaks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := conn.Search(context.Background(), searchIn("q", 30*time.Second))
			if err != nil {
				t.Error(err)
				return
			}
			peak, _ := resp.Diagnostics["peak_in_flight"].(float64)
			peaks[i] = peak
		}()
	}
	wg.Wait()

	for i, peak := range peaks {
		if peak > 1 {
			t.Fatalf("request %d observed %v concurrent requests inside an adapter that declared 1", i, peak)
		}
	}
}

// Probing every source on every query would cost more than the query. The TTL
// is what makes health affordable.
func TestHealthProbesAreCachedForTheirTTL(t *testing.T) {
	f := newFixture(modeOK)
	conn := connectFixture(t, f, adapter.Options{HealthTTL: 100 * time.Millisecond})
	ctx := context.Background()

	for range 5 {
		if _, err := conn.Health(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.probeCount(); n != 1 {
		t.Fatalf("adapter was probed %d times inside one TTL, want 1", n)
	}

	time.Sleep(150 * time.Millisecond)
	if _, err := conn.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.probeCount(); n != 2 {
		t.Fatalf("probes = %d, want a fresh probe after the TTL elapsed", n)
	}
}

// Concurrent callers must collapse onto one probe, or a burst of queries would
// stampede the source at exactly the moment it is already busy.
func TestConcurrentHealthProbesCollapse(t *testing.T) {
	f := newFixture(modeOK)
	conn := connectFixture(t, f, adapter.Options{HealthTTL: time.Minute})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := conn.Health(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if n := f.probeCount(); n != 1 {
		t.Fatalf("probes = %d, want 1", n)
	}
}

// Expansion never silently returns a different revision or a nearby record.
// A source that changed incompatibly says so with a code the caller can match.
func TestExpiredLocatorFailsExplicitly(t *testing.T) {
	conn := connectFixture(t, newFixture(modeOK), adapter.Options{})

	ev, err := conn.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "fixture", Local: "gone"},
		Detail:   recall.DetailFull,
		Budget:   1024,
		Deadline: time.Now().Add(5 * time.Second).UTC(),
	})
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("err = %v, want locator_expired", err)
	}
	if ev.Content != "" {
		t.Fatalf("a failed expansion returned content: %q", ev.Content)
	}
}

// Budget is a hard limit, and truncation names the boundary that applied so a
// caller can tell a budget cut from a source-side one.
func TestExpansionTruncatesAtItsBudget(t *testing.T) {
	conn := connectFixture(t, newFixture(modeOK), adapter.Options{})

	ev, err := conn.Expand(context.Background(), recall.ExpandRequest{
		Locator:  recall.Locator{SourceID: "fixture", Local: "rec-1"},
		Detail:   recall.DetailExcerpt,
		Budget:   8,
		Deadline: time.Now().Add(5 * time.Second).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Truncated || ev.TruncationBoundary != "budget_bytes" {
		t.Fatalf("evidence = %+v, want a named truncation boundary", ev)
	}
	if int64(len(ev.Content)) > 8 {
		t.Fatalf("content of %d bytes exceeded the budget", len(ev.Content))
	}
}

// The invariant, stated as a test: nothing that went wrong can look like a
// source that simply had no matches. Every way an adapter can fail is checked
// against the same two assertions.
func TestNoFailurePathProducesEmptySuccess(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T) (recall.SearchResponse, error)
		want recall.SearchOutcome
	}{
		{
			name: "command does not exist",
			want: recall.SearchUnavailable,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				ext := adapter.NewExternal(adapter.Spec{
					Name:    "missing",
					Command: "/nonexistent/recall-adapter-that-is-not-there",
					Config: adapter.Config{
						ProtocolVersionMin: protocol.MinVersion,
						ProtocolVersionMax: protocol.MaxVersion,
						Workdir:            t.TempDir(),
						SourceID:           "tasks",
					},
				})
				t.Cleanup(func() { _ = ext.Close() })
				return ext.Search(context.Background(), searchIn("q", 5*time.Second))
			},
		},
		{
			name: "adapter exits before the handshake",
			want: recall.SearchUnavailable,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				ext := externalFixture(t, modeCrash, adapter.Options{HandshakeTimeout: 3 * time.Second})
				return ext.Search(context.Background(), searchIn("q", 5*time.Second))
			},
		},
		{
			name: "protocol version cannot be agreed",
			want: recall.SearchUnavailable,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				ext := externalFixture(t, modeBadVersion, adapter.Options{})
				return ext.Search(context.Background(), searchIn("q", 5*time.Second))
			},
		},
		{
			name: "adapter never answers",
			want: recall.SearchTimeout,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				ext := externalFixture(t, modeHang, adapter.Options{
					CancelGrace: 50 * time.Millisecond,
					TermGrace:   500 * time.Millisecond,
				})
				return ext.Search(context.Background(), searchIn("q", 150*time.Millisecond))
			},
		},
		{
			name: "permission is refused",
			want: recall.SearchDenied,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				conn := connectFixture(t, newFixture(modeDenied), adapter.Options{})
				return conn.Search(context.Background(), searchIn("q", 5*time.Second))
			},
		},
		{
			name: "the adapter is already closed",
			want: recall.SearchUnavailable,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				ext := externalFixture(t, modeOK, adapter.Options{})
				if err := ext.Close(); err != nil {
					t.Fatal(err)
				}
				return ext.Search(context.Background(), searchIn("q", 5*time.Second))
			},
		},
		{
			name: "the deadline has already elapsed",
			want: recall.SearchTimeout,
			run: func(t *testing.T) (recall.SearchResponse, error) {
				conn := connectFixture(t, newFixture(modeOK), adapter.Options{
					CancelGrace: 20 * time.Millisecond,
				})
				return conn.Search(context.Background(), searchIn("q", -time.Second))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := tt.run(t)
			if err == nil {
				t.Fatal("the failure was reported as success")
			}
			if resp.Outcome == recall.SearchSuccess || resp.Outcome == recall.SearchPartial {
				t.Fatalf("outcome = %s, which reads as a source that answered", resp.Outcome)
			}
			if len(resp.Candidates) != 0 {
				t.Fatalf("a failed search carried %d candidates", len(resp.Candidates))
			}
			if resp.Outcome != tt.want {
				t.Errorf("outcome = %s, want %s", resp.Outcome, tt.want)
			}
			if _, ok := resp.Diagnostics["reason"]; !ok {
				t.Errorf("a failure must carry a reason: %+v", resp.Diagnostics)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		outcome recall.SearchOutcome
		reason  string
	}{
		{"unavailable", protocol.ErrSourceUnavailable, recall.SearchUnavailable, "unreachable"},
		{"denied", protocol.ErrSourceDenied, recall.SearchDenied, "denied"},
		{"as_of", protocol.ErrAsOfUnsupported, recall.SearchFailed, "as_of_unsupported"},
		{"budget", protocol.ErrBudgetExceeded, recall.SearchFailed, "budget_exceeded"},
		{"not configured", protocol.ErrSourceNotConfigured, recall.SearchFailed, "source_not_configured"},
		{"locator expired", protocol.ErrLocatorExpired, recall.SearchFailed, "locator_expired"},
		{"locator unknown", protocol.ErrLocatorUnknown, recall.SearchFailed, "locator_unknown"},
		{"stream closed", protocol.ErrClosed, recall.SearchUnavailable, "unreachable"},
		{"context deadline", context.DeadlineExceeded, recall.SearchTimeout, "deadline_exceeded"},
		{
			"answered cancel", &protocol.CallTimeout{Acknowledged: true, Cause: context.DeadlineExceeded},
			recall.SearchTimeout, "deadline_exceeded",
		},
		{
			// The distinction the supervisor acts on: no answer at all means
			// the process was signalled, and the report should say so.
			"ignored cancel", &protocol.CallTimeout{Cause: context.DeadlineExceeded},
			recall.SearchTimeout, "deadline_exceeded_unresponsive",
		},
		{"spawn", &adapter.SpawnError{Name: "x", Command: "y", Err: errors.New("no")}, recall.SearchUnavailable, "spawn_failed"},
		{"anything else", errors.New("boom"), recall.SearchFailed, "adapter_error"},
		{"nil", nil, recall.SearchFailed, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, reason := adapter.Classify(tt.err)
			if outcome != tt.outcome || reason != tt.reason {
				t.Errorf("got (%s, %s), want (%s, %s)", outcome, reason, tt.outcome, tt.reason)
			}
			// Whatever the failure, the rendered response is never a success.
			if got := adapter.FailedSearch(tt.err); got.Outcome == recall.SearchSuccess {
				t.Error("FailedSearch produced a successful response")
			}
		})
	}
}

// A failed probe is never healthy, and its coverage is unknown rather than
// complete: an unreachable source proves nothing about what it contains.
func TestUnhealthyProbeIsNeverHealthy(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want recall.HealthStatus
	}{
		{"unreachable", protocol.ErrSourceUnavailable, recall.HealthUnavailable},
		{"denied", protocol.ErrSourceDenied, recall.HealthDenied},
		{"timeout", context.DeadlineExceeded, recall.HealthUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := adapter.Unhealthy(tt.err)
			if h.Status != tt.want {
				t.Errorf("status = %s, want %s", h.Status, tt.want)
			}
			if h.Usable() {
				t.Error("a failed probe must not report a usable source")
			}
			if h.Coverage != recall.IndexUnknown {
				t.Errorf("coverage = %s, want unknown", h.Coverage)
			}
		})
	}
}
