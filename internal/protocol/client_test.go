package protocol_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// stubHandler is an adapter that does whatever the test needs. Every hook
// defaults to a well-formed answer so a test only states the behavior it cares
// about.
type stubHandler struct {
	search  func(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error)
	expand  func(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error)
	health  func(ctx context.Context) (recall.Health, error)
	initErr error

	maxConcurrency int

	mu       sync.Mutex
	inFlight int
	peak     int
}

func (h *stubHandler) Initialize(_ context.Context, p protocol.InitializeParams) (recall.Manifest, error) {
	if h.initErr != nil {
		return recall.Manifest{}, h.initErr
	}
	version, err := protocol.NegotiateVersion(p.ProtocolVersionMin, p.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       "stub/0.1.0",
		DisplayName:     "Stub",
		RecordTypes:     []recall.RecordType{recall.RecordTask},
		QueryModes:      []recall.QueryMode{recall.QueryLexical},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessLive},
		AsOfSupport:     recall.AsOfNone,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		MaxConcurrency:  h.maxConcurrency,
		Sensitivity:     recall.SensitivityInternal,
	}, nil
}

func (h *stubHandler) enter() func() {
	h.mu.Lock()
	h.inFlight++
	if h.inFlight > h.peak {
		h.peak = h.inFlight
	}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		h.inFlight--
		h.mu.Unlock()
	}
}

func (h *stubHandler) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	defer h.enter()()
	if h.search != nil {
		return h.search(ctx, req)
	}
	// Echo the query so a caller can prove it received its own answer.
	return recall.SearchResponse{Outcome: recall.SearchSuccess, SourceWatermark: req.Query}, nil
}

func (h *stubHandler) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	if h.expand != nil {
		return h.expand(ctx, req)
	}
	return recall.ExpandResponse{Content: req.Locator.Local}, nil
}

func (h *stubHandler) Health(ctx context.Context) (recall.Health, error) {
	if h.health != nil {
		return h.health(ctx)
	}
	return recall.Health{
		Status:    recall.HealthHealthy,
		CheckedAt: time.Now().UTC(),
		Coverage:  recall.IndexComplete,
	}, nil
}

func (h *stubHandler) Refresh(context.Context, protocol.RefreshParams) (recall.Health, error) {
	return recall.Health{Status: recall.HealthHealthy, Coverage: recall.IndexComplete}, nil
}

func (h *stubHandler) Shutdown(context.Context) error { return nil }

// serveStub wires a client to a handler over two pipes: the same request path a
// subprocess uses, without the process.
func serveStub(t *testing.T, h protocol.Handler, opt protocol.ClientOptions) *protocol.Client {
	t.Helper()

	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = protocol.Serve(ctx, serverReads, serverWrites, h)
		_ = serverWrites.Close()
	}()

	opt.Closer = clientWrites
	c, err := protocol.NewClient(clientReads, clientWrites, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		cancel()
		_ = serverReads.Close()
		<-served
		c.Wait()
	})
	return c
}

func searchReq(query string, in time.Duration) recall.SearchRequest {
	return recall.SearchRequest{
		Query:    query,
		Limit:    10,
		Deadline: time.Now().Add(in).UTC(),
	}
}

func TestRoundTrip(t *testing.T) {
	c := serveStub(t, &stubHandler{}, protocol.ClientOptions{})
	ctx := context.Background()

	var manifest recall.Manifest
	err := c.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
	}, &manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProtocolVersion != protocol.MaxVersion {
		t.Errorf("negotiated %d, want %d", manifest.ProtocolVersion, protocol.MaxVersion)
	}

	var resp recall.SearchResponse
	if err := c.Call(ctx, protocol.MethodSearch, searchReq("hello", time.Second), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Outcome != recall.SearchSuccess || resp.SourceWatermark != "hello" {
		t.Fatalf("resp = %+v", resp)
	}
}

// Concurrent in-flight requests are the case correlation exists for. The
// handler answers in the opposite order to the requests, so a client that
// matched by arrival order would hand every caller the wrong answer.
func TestConcurrentCallsNeverMisCorrelate(t *testing.T) {
	release := make(chan struct{})
	h := &stubHandler{
		search: func(_ context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
			// Hold every request until all of them have arrived, then let them
			// finish in whatever order the scheduler picks.
			<-release
			return recall.SearchResponse{Outcome: recall.SearchSuccess, SourceWatermark: req.Query}, nil
		},
	}
	c := serveStub(t, h, protocol.ClientOptions{})

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			query := "q-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + "-" + itoa(i)
			var resp recall.SearchResponse
			if err := c.Call(context.Background(), protocol.MethodSearch,
				searchReq(query, 10*time.Second), &resp); err != nil {
				errs[i] = err
				return
			}
			if resp.SourceWatermark != query {
				errs[i] = errors.New("got answer for " + resp.SourceWatermark + ", asked " + query)
			}
		}()
	}
	// Give every call time to be written before any is answered.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Recall's codes are the contract's vocabulary. A caller matches on them with
// errors.Is regardless of the message the adapter attached.
func TestRecallErrorCodesSurviveTheWire(t *testing.T) {
	tests := []struct {
		name string
		err  *protocol.Error
		want error
	}{
		{"unavailable", protocol.Errorf(protocol.CodeSourceUnavailable, "connect: refused"), protocol.ErrSourceUnavailable},
		{"denied", protocol.Errorf(protocol.CodeSourceDenied, "no access"), protocol.ErrSourceDenied},
		{"locator unknown", protocol.Errorf(protocol.CodeLocatorUnknown, "bad shape"), protocol.ErrLocatorUnknown},
		{"locator expired", protocol.Errorf(protocol.CodeLocatorExpired, "revision moved"), protocol.ErrLocatorExpired},
		{"not configured", protocol.Errorf(protocol.CodeSourceNotConfigured, "no such source"), protocol.ErrSourceNotConfigured},
		{"as_of", protocol.Errorf(protocol.CodeAsOfUnsupported, "live only"), protocol.ErrAsOfUnsupported},
		{"budget", protocol.Errorf(protocol.CodeBudgetExceeded, "too small"), protocol.ErrBudgetExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &stubHandler{
				search: func(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
					return recall.SearchResponse{}, tt.err
				},
			}
			c := serveStub(t, h, protocol.ClientOptions{})

			var resp recall.SearchResponse
			err := c.Call(context.Background(), protocol.MethodSearch, searchReq("q", time.Second), &resp)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			var perr *protocol.Error
			if !errors.As(err, &perr) {
				t.Fatalf("err = %v, want a *protocol.Error", err)
			}
			if perr.Message != tt.err.Message {
				t.Errorf("message = %q, want %q", perr.Message, tt.err.Message)
			}
			if resp.Outcome == recall.SearchSuccess {
				t.Error("a failed call must not leave a successful response behind")
			}
		})
	}
}

// Cancellation is advisory but observable: a well-behaved adapter sees its
// context end and answers, which is how the core learns the process is alive
// rather than wedged.
func TestDeadlineCancelsAnAttentiveAdapter(t *testing.T) {
	observed := make(chan struct{}, 1)
	h := &stubHandler{
		search: func(ctx context.Context, _ recall.SearchRequest) (recall.SearchResponse, error) {
			<-ctx.Done()
			select {
			case observed <- struct{}{}:
			default:
			}
			return recall.SearchResponse{}, protocol.Errorf(protocol.CodeBudgetExceeded, "cancelled")
		},
	}
	c := serveStub(t, h, protocol.ClientOptions{CancelGrace: 2 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var resp recall.SearchResponse
	err := c.Call(ctx, protocol.MethodSearch, searchReq("slow", time.Hour), &resp)

	var timeout *protocol.CallTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want a CallTimeout", err)
	}
	if !timeout.Acknowledged {
		t.Error("an adapter that answered the cancel must be reported as acknowledged")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the cause should stay reachable, got %v", err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Error("the adapter never observed the cancellation")
	}
}

// A wedged adapter is the case that matters: no answer at all. The timeout is
// reported as unacknowledged, which is the signal a supervisor escalates on.
func TestWedgedAdapterReportsAnUnacknowledgedTimeout(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	h := &stubHandler{
		search: func(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
			<-stop
			return recall.SearchResponse{Outcome: recall.SearchSuccess}, nil
		},
	}
	c := serveStub(t, h, protocol.ClientOptions{CancelGrace: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var resp recall.SearchResponse
	err := c.Call(ctx, protocol.MethodSearch, searchReq("wedged", time.Hour), &resp)

	var timeout *protocol.CallTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want a CallTimeout", err)
	}
	if timeout.Acknowledged {
		t.Error("no answer arrived, so the timeout must not be acknowledged")
	}
	if resp.Outcome != "" {
		t.Fatalf("a timed-out call wrote a result: %+v", resp)
	}
}

// stderr is free-form adapter logging. Nothing written there may complete a
// request, even when it is a perfectly formed protocol frame.
func TestStderrIsCapturedAndNeverParsed(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	h := &stubHandler{
		search: func(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
			<-stop
			return recall.SearchResponse{Outcome: recall.SearchSuccess}, nil
		},
	}
	diag := protocol.NewDiagnostics()
	c := serveStub(t, h, protocol.ClientOptions{
		Diagnostics: diag,
		CancelGrace: 20 * time.Millisecond,
	})

	// Exactly the frame that would answer the first request, on the wrong
	// channel.
	forged := `{"jsonrpc":"2.0","id":1,"result":{"candidates":[],"outcome":"success"}}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		diag.Capture(strings.NewReader("starting up\n" + forged + "\n"))
	}()
	<-done

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	var resp recall.SearchResponse
	err := c.Call(ctx, protocol.MethodSearch, searchReq("q", time.Hour), &resp)
	var timeout *protocol.CallTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("stderr answered a request: err = %v, resp = %+v", err, resp)
	}

	lines := diag.Lines()
	if len(lines) != 2 || lines[0] != "starting up" || lines[1] != forged {
		t.Fatalf("stderr was not captured verbatim: %q", lines)
	}
}

// A response nobody is waiting for is dropped, not handed to the next caller.
// Delivering it would be the exact mis-correlation this design forbids.
func TestUnmatchedResponseIsRecordedAndDropped(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()

	diag := protocol.NewDiagnostics()
	c, err := protocol.NewClient(clientReads, clientWrites, protocol.ClientOptions{
		Diagnostics: diag,
		Closer:      clientWrites,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = serverWrites.Close()
		_ = serverReads.Close()
		c.Wait()
	})

	go func() {
		br := bufio.NewReader(serverReads)
		// Answer a request that was never made, then the real one.
		_, _ = serverWrites.Write([]byte(`{"jsonrpc":"2.0","id":999,"result":{"candidates":[],"outcome":"failed"}}` + "\n"))
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg protocol.Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return
		}
		out, _ := json.Marshal(recall.SearchResponse{Outcome: recall.SearchSuccess, SourceWatermark: "real"})
		reply, _ := json.Marshal(protocol.NewResult(*msg.ID, out))
		_, _ = serverWrites.Write(append(reply, '\n'))
	}()

	var resp recall.SearchResponse
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, protocol.MethodSearch, searchReq("q", time.Minute), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SourceWatermark != "real" {
		t.Fatalf("call received the unmatched response: %+v", resp)
	}
	if diag.Violations() == 0 {
		t.Error("the unmatched response should have been recorded as a violation")
	}
}

// A stream that dies mid-request must wake every waiter. A call left blocked on
// a dead adapter would hold a query open past its budget.
func TestBrokenStreamWakesPendingCalls(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()
	t.Cleanup(func() { _ = serverReads.Close() })

	c, err := protocol.NewClient(clientReads, clientWrites, protocol.ClientOptions{Closer: clientWrites})
	if err != nil {
		t.Fatal(err)
	}
	// A peer that reads but never answers: the request leaves, nothing returns.
	go func() { _, _ = io.Copy(io.Discard, serverReads) }()

	done := make(chan error, 1)
	go func() {
		var resp recall.SearchResponse
		done <- c.Call(context.Background(), protocol.MethodSearch, searchReq("q", time.Hour), &resp)
	}()

	time.Sleep(20 * time.Millisecond)
	_ = serverWrites.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a dead stream must not look like a successful call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call did not wake when the stream died")
	}
	c.Wait()
}

func TestHandshakeRefusesAnUnsatisfiableRange(t *testing.T) {
	c := serveStub(t, &stubHandler{}, protocol.ClientOptions{})

	var manifest recall.Manifest
	err := c.Call(context.Background(), protocol.MethodInitialize, protocol.InitializeParams{
		ProtocolVersionMin: 99,
		ProtocolVersionMax: 100,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
	}, &manifest)
	if err == nil {
		t.Fatal("an unsatisfiable range must fail the handshake, not degrade")
	}
	if manifest.ProtocolVersion != 0 {
		t.Fatalf("a failed handshake produced a manifest: %+v", manifest)
	}
}

func TestNegotiateVersion(t *testing.T) {
	tests := []struct {
		name     string
		min, max int
		want     int
		wantErr  bool
	}{
		{"exact", 1, 1, 1, false},
		{"range covers us", 1, 5, protocol.MaxVersion, false},
		{"above us", 2, 9, 0, true},
		{"below us", 0, 0, 0, true},
		{"inverted", 5, 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := protocol.NegotiateVersion(tt.min, tt.max)
			if tt.wantErr {
				var verr *protocol.VersionError
				if !errors.As(err, &verr) {
					t.Fatalf("err = %v, want a VersionError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// The adapter is the server. A frame asking the core to do something is
// recorded and ignored: retrieved content never steers the core.
func TestAdapterCannotIssueRequestsToTheCore(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	serverReads, clientWrites := io.Pipe()
	t.Cleanup(func() { _ = serverReads.Close() })

	diag := protocol.NewDiagnostics()
	c, err := protocol.NewClient(clientReads, clientWrites, protocol.ClientOptions{
		Diagnostics: diag,
		Closer:      clientWrites,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(); _ = serverWrites.Close(); c.Wait() })

	if _, err := serverWrites.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"recall/search","params":{}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for diag.Violations() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if diag.Violations() == 0 {
		t.Fatal("an adapter-issued request should be recorded as a violation")
	}
}

// blockedWriter never completes a write, standing in for an adapter that has
// stopped reading its own stdin and let the pipe buffer fill.
type blockedWriter struct{ release chan struct{} }

func (w *blockedWriter) Write([]byte) (int, error) {
	<-w.release
	return 0, io.ErrClosedPipe
}

func (w *blockedWriter) Close() error { return nil }

// Not reading is a cheaper way to wedge the core than not answering, and it
// used to work: the encode sat in front of the ctx.Done branch, so no cancel
// was sent and no timeout was ever returned. The whole supervision ladder was
// behind a blocking write.
func TestCallTimesOutWhenTheAdapterStopsReading(t *testing.T) {
	clientReads, serverWrites := io.Pipe()
	t.Cleanup(func() { _ = serverWrites.Close() })

	stuck := &blockedWriter{release: make(chan struct{})}
	// Releasing the write is what terminating the process would do: stdin
	// closes and the stuck write fails.
	t.Cleanup(func() { close(stuck.release) })

	c, err := protocol.NewClient(clientReads, stuck, protocol.ClientOptions{Closer: stuck})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = c.Call(ctx, protocol.MethodSearch, map[string]any{
		"query":    "q",
		"filters":  map[string]any{},
		"limit":    5,
		"deadline": time.Now().Add(time.Second).Format(time.RFC3339Nano),
	}, &struct{}{})
	elapsed := time.Since(start)

	var timeout *protocol.CallTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v (%T), want CallTimeout", err, err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("call took %v; the deadline must bound the write too", elapsed)
	}
}
