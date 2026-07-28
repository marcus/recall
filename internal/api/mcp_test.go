package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/pointer"
	"github.com/marcus/recall/internal/recall"
)

func TestMCPReturnsTheSameTypedQueryExpandAndRefreshResults(t *testing.T) {
	wantQuery := sampleQueryResponse()
	wantExpand := recall.ExpandResponse{
		Content: "the evidence", Provenance: "notes.md#L1-2", SourceRevision: "abc123",
	}
	wantRefresh := recall.RefreshResponse{
		Outcome: recall.RefreshSucceeded,
		Sources: []recall.RefreshSourceOutcome{{
			SourceID: "notes", Status: recall.RefreshSourceRefreshed,
			Health: &recall.Health{Status: recall.HealthHealthy, Coverage: recall.IndexComplete, IndexGeneration: "gen-2"},
		}},
	}
	core := &stubCore{
		query:   wantQuery,
		expand:  wantExpand,
		refresh: wantRefresh,
		sources: Listing{Payload: map[string]any{"profile": "work", "sources": []any{}}, Status: StatusOK},
	}
	responses := runMCP(t, core,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall_query","arguments":{"query":"decision","limit":7,"sources":["notes"],"record_types":["document"],"project":"recall","entities":["Marcus"],"since":"2026-07-01T00:00:00Z","until":"2026-07-25T00:00:00Z","as_of":"2026-07-24T12:00:00Z","budget_tokens":900,"budget_ms":750}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall_expand","arguments":{"locator":"notes:record-1#L1-2"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"recall_sources","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"recall_refresh","arguments":{"source_id":"notes","full":true}}}`,
	)

	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	decodeResult(t, responses["1"], &initialized)
	if initialized.ProtocolVersion != MCPProtocolVersion {
		t.Fatalf("protocol version = %q", initialized.ProtocolVersion)
	}

	var listed struct {
		Tools []Tool `json:"tools"`
	}
	decodeResult(t, responses["2"], &listed)
	if len(listed.Tools) != 4 {
		t.Fatalf("tools/list returned %d tools", len(listed.Tools))
	}

	var query callToolResult
	decodeResult(t, responses["3"], &query)
	if query.IsError {
		t.Fatalf("answered query marked error: %+v", query)
	}
	// The default structured half is the pointer projection, not the response:
	// the consumer is a model paying context for every field, which is the
	// argument the CLI already applied to `--json`.
	assertSameJSON(t, query.StructuredContent, pointer.Project(wantQuery))
	if !strings.Contains(query.Content[0].Text, "notes:record-1#L1-2") {
		t.Fatalf("query text omitted round-trippable locator: %s", query.Content[0].Text)
	}
	if core.lastQuery.Limit != 7 || core.lastQuery.Budget.LatencyMS != 750 ||
		core.lastQuery.Budget.ResponseTokens != 900 || core.lastQuery.Scope == nil ||
		core.lastQuery.Scope.Project != "recall" || len(core.lastQuery.Scope.SourceIDs) != 1 ||
		len(core.lastQuery.Scope.Entities) != 1 || core.lastQuery.Scope.Entities[0] != "Marcus" ||
		core.lastQuery.AsOf == nil {
		t.Fatalf("MCP query lost typed request fields: %+v", core.lastQuery)
	}

	var expand callToolResult
	decodeResult(t, responses["4"], &expand)
	assertSameJSON(t, expand.StructuredContent, wantExpand)
	if !strings.HasSuffix(expand.Content[0].Text, "the evidence") {
		t.Fatalf("expand text omitted evidence: %s", expand.Content[0].Text)
	}

	var sources callToolResult
	decodeResult(t, responses["5"], &sources)
	assertSameJSON(t, sources.StructuredContent, core.sources.Payload)

	var refresh callToolResult
	decodeResult(t, responses["6"], &refresh)
	if refresh.IsError {
		t.Fatalf("successful refresh marked error: %+v", refresh)
	}
	assertSameJSON(t, refresh.StructuredContent, wantRefresh)
	if core.lastRefresh.SourceID != "notes" || !core.lastRefresh.Full {
		t.Fatalf("MCP refresh lost request fields: %+v", core.lastRefresh)
	}

}

// explain is the diagnostic tier, and it is the complete serialization — byte
// for byte what this surface returned before it projected, so a caller that
// needs a dropped field has an exact migration rather than a reconstruction.
func TestMCPQueryExplainIsTheCompleteSerialization(t *testing.T) {
	want := sampleQueryResponse()
	responses := runMCP(t, &stubCore{query: want},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_query","arguments":{"query":"decision","explain":true}}}`,
	)
	var got callToolResult
	decodeResult(t, responses["2"], &got)
	assertSameJSON(t, got.StructuredContent, want)
}

// The budget is denominated in the tier the call will actually return, or a
// model asking for pointers is shaped for a response several times the size and
// receives fewer results than it asked for. One query per session: requests are
// handled concurrently, so a stub's last-request field cannot be read across
// two of them.
func TestMCPQueryIsPricedForTheTierItReturns(t *testing.T) {
	for _, tc := range []struct {
		args string
		want recall.ResponseSurface
	}{
		{`{"query":"decision"}`, recall.SurfaceTool},
		{`{"query":"decision","explain":true}`, recall.SurfaceToolExplained},
		{`{"query":"decision","explain":false}`, recall.SurfaceTool},
	} {
		core := &stubCore{query: sampleQueryResponse()}
		runMCP(t, core,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_query","arguments":`+tc.args+`}}`,
		)
		if got := core.lastQuery.Budget.Surface; got != tc.want {
			t.Errorf("arguments %s priced as %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestMCPRefreshRejectsUnknownArgumentsStrictly(t *testing.T) {
	responses := runMCP(t, &stubCore{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_refresh","arguments":{"source":"docs"}}}`,
	)
	var got callToolResult
	decodeResult(t, responses["2"], &got)
	if !got.IsError || !strings.Contains(got.Content[0].Text, "unknown field") {
		t.Fatalf("unknown refresh argument not refused: %+v", got)
	}
}

func TestMCPFailedRefreshKeepsTypedOutcomeAndSetsToolError(t *testing.T) {
	want := recall.RefreshResponse{
		Outcome: recall.RefreshFailed,
		Sources: []recall.RefreshSourceOutcome{{
			SourceID: "docs", Status: recall.RefreshSourceFailed, Reason: recall.RefreshOperationFailed,
		}},
	}
	responses := runMCP(t, &stubCore{refresh: want},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_refresh","arguments":{"source_id":"docs"}}}`,
	)
	var got callToolResult
	decodeResult(t, responses["2"], &got)
	if !got.IsError {
		t.Fatal("failed refresh was presented as a successful tool call")
	}
	assertSameJSON(t, got.StructuredContent, want)
}

func TestMCPFailedOutcomeCannotLookLikeEmptySuccess(t *testing.T) {
	core := &stubCore{query: recall.QueryResponse{
		Outcome: recall.OutcomeFailed, Coverage: recall.CoverageDegraded,
		SourceOutcomes: []recall.SourceReport{{
			SourceID: "notes", Outcome: recall.SearchUnavailable, Reason: "unreachable",
		}},
	}}
	responses := runMCP(t, core,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"q","method":"tools/call","params":{"name":"recall_query","arguments":{"query":"x"}}}`,
	)
	var got callToolResult
	decodeResult(t, responses[`"q"`], &got)
	if !got.IsError {
		t.Fatal("failed query was presented as a successful tool call")
	}
	if !strings.Contains(got.Content[0].Text, "not evidence that nothing matched") {
		t.Fatalf("failed query text loses meaning: %s", got.Content[0].Text)
	}
}

func TestMCPCancellationNotificationCancelsInFlightCoreCall(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	core := &stubCore{queryFunc: func(ctx context.Context, _ recall.QueryRequest) (recall.QueryResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return recall.QueryResponse{}, ctx.Err()
	}}

	pr, pw := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ServeMCP(t.Context(), pr, &output, MCPOptions{Core: core})
	}()
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n"+
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":99,\"method\":\"tools/call\",\"params\":{\"name\":\"recall_query\",\"arguments\":{\"query\":\"slow\"}}}\n")
	<-started
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":99,\"reason\":\"host stopped\"}}\n")

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("MCP cancellation did not reach the core")
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}

	var response mcpResponse
	scan := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scan.Scan() {
		var candidate mcpResponse
		if err := json.Unmarshal(scan.Bytes(), &candidate); err != nil {
			t.Fatalf("decode cancellation response: %v\n%s", err, output.String())
		}
		if string(candidate.ID) == "99" {
			response = candidate
		}
	}
	if len(response.ID) == 0 {
		t.Fatalf("no cancellation response for request 99\n%s", output.String())
	}
	var result callToolResult
	raw, _ := json.Marshal(response.Result)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "could not be run") {
		t.Fatalf("cancelled call did not become actionable tool error: %+v", result)
	}
}

func TestMCPServerContextCancellationUnblocksIdleStdio(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- ServeMCP(ctx, pr, io.Discard, MCPOptions{Core: &stubCore{}})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeMCP after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation left MCP blocked on idle input")
	}
}

func TestMCPConcurrencySaturationIsRejectedWithoutStartingWork(t *testing.T) {
	started := make(chan struct{})
	secondStarted := make(chan struct{}, 1)
	core := &stubCore{queryFunc: func(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
		if req.Query == "second" {
			secondStarted <- struct{}{}
			return recall.QueryResponse{}, nil
		}
		close(started)
		<-ctx.Done()
		return recall.QueryResponse{}, ctx.Err()
	}}
	pr, pw := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- ServeMCP(t.Context(), pr, &output, MCPOptions{
			Core: core, MaxInFlight: 1, ShutdownTimeout: time.Second,
		})
	}()
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":\"init\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n"+
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"+
			"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"recall_query\",\"arguments\":{\"query\":\"first\"}}}\n")
	<-started
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"recall_query\",\"arguments\":{\"query\":\"second\"}}}\n"+
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":1}}\n")
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondStarted:
		t.Fatal("saturated request reached the core")
	default:
	}
	responses := responseMap(t, output.Bytes())
	if responses["2"].Error == nil || responses["2"].Error.Code != codeServerBusy {
		t.Fatalf("saturated response: %+v", responses["2"])
	}
}

func TestMCPStdinEOFCancelsInflightWork(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	core := &stubCore{queryFunc: func(ctx context.Context, _ recall.QueryRequest) (recall.QueryResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return recall.QueryResponse{}, ctx.Err()
	}}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- ServeMCP(t.Context(), pr, io.Discard, MCPOptions{
			Core: core, ShutdownTimeout: time.Second,
		})
	}()
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":\"init\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n"+
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"+
			"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"recall_query\",\"arguments\":{\"query\":\"slow\"}}}\n")
	<-started
	_ = pw.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stdin EOF did not cancel in-flight work")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMCPShutdownDoesNotWaitForeverForNonCooperativeCore(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	core := &stubCore{queryFunc: func(context.Context, recall.QueryRequest) (recall.QueryResponse, error) {
		close(started)
		<-release
		return recall.QueryResponse{}, nil
	}}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	const shutdown = 30 * time.Millisecond
	go func() {
		done <- ServeMCP(t.Context(), pr, io.Discard, MCPOptions{
			Core: core, ShutdownTimeout: shutdown,
		})
	}()
	_, _ = io.WriteString(pw,
		"{\"jsonrpc\":\"2.0\",\"id\":\"init\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n"+
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"+
			"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"recall_query\",\"arguments\":{\"query\":\"stuck\"}}}\n")
	<-started
	began := time.Now()
	_ = pw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * shutdown):
		t.Fatal("shutdown waited indefinitely for non-cooperative core")
	}
	if elapsed := time.Since(began); elapsed > 8*shutdown {
		t.Fatalf("shutdown took %s, configured %s", elapsed, shutdown)
	}
	close(release)
}

func TestMCPBlockedOutputCannotDefeatShutdownBound(t *testing.T) {
	output := newBlockingWriteCloser()
	input := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n")
	const shutdown = 30 * time.Millisecond
	started := time.Now()
	if err := ServeMCP(t.Context(), input, output, MCPOptions{
		Core: &stubCore{}, ShutdownTimeout: shutdown,
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 8*shutdown {
		t.Fatalf("blocked output held shutdown for %s", elapsed)
	}
	select {
	case <-output.writeStarted:
	default:
		t.Fatal("test did not exercise a blocked encoder write")
	}
	if !output.closed.Load() {
		t.Fatal("shutdown timeout did not close blocked output")
	}
}

func TestMCPSaturatedResponseQueueCannotDefeatShutdownBound(t *testing.T) {
	const (
		maxInFlight = 1
		pingCount   = 10_000
		shutdown    = 30 * time.Millisecond
	)

	output := newBlockingWriteCloser()
	input, client := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- ServeMCP(ctx, input, output, MCPOptions{
			Core:            &stubCore{},
			MaxInFlight:     maxInFlight,
			ShutdownTimeout: shutdown,
		})
	}()

	saturated := make(chan struct{})
	producerDone := make(chan error, 1)
	go func() {
		if _, err := io.WriteString(client,
			"{\"jsonrpc\":\"2.0\",\"id\":\"init\",\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}\n"+
				"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"); err != nil {
			producerDone <- err
			return
		}
		// One response is blocked in output.Write. The next
		// maxInFlight+8 responses fill the bounded queue, and the following
		// ping is consumed from stdin before the server blocks enqueueing its
		// response. Reaching that write proves the exact saturation state.
		for i := 0; i < pingCount; i++ {
			if _, err := fmt.Fprintf(client,
				"{\"jsonrpc\":\"2.0\",\"id\":\"ping-%d\",\"method\":\"ping\"}\n", i); err != nil {
				producerDone <- err
				return
			}
			if i == maxInFlight+8 {
				close(saturated)
			}
		}
		producerDone <- nil
	}()

	select {
	case <-output.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("test did not block the output writer")
	}
	select {
	case <-saturated:
	case <-time.After(time.Second):
		t.Fatal("test did not saturate the response queue")
	}

	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * shutdown):
		t.Fatal("saturated response queue prevented bounded shutdown")
	}
	if elapsed := time.Since(began); elapsed > 8*shutdown {
		t.Fatalf("saturated response queue held shutdown for %s", elapsed)
	}
	if !output.closed.Load() {
		t.Fatal("shutdown timeout did not close saturated blocked output")
	}

	_ = client.Close()
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("input producer remained blocked after shutdown")
	}
}

type blockingWriteCloser struct {
	writeStarted chan struct{}
	unblock      chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
	closed       atomic.Bool
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{
		writeStarted: make(chan struct{}),
		unblock:      make(chan struct{}),
	}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.unblock
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.closed.Store(true)
	w.closeOnce.Do(func() { close(w.unblock) })
	return nil
}

func TestMCPLifecycleRequiresInitializeThenInitialized(t *testing.T) {
	core := &stubCore{}
	responses := runMCP(t, core,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
	)
	if responses["1"].Error == nil || !strings.Contains(responses["1"].Error.Message, "initialize") {
		t.Fatalf("pre-initialize tools/list response: %+v", responses["1"])
	}
	if responses["3"].Error == nil || !strings.Contains(responses["3"].Error.Message, "notifications/initialized") {
		t.Fatalf("pre-initialized tools/list response: %+v", responses["3"])
	}
	if responses["4"].Error != nil {
		t.Fatalf("ready tools/list: %+v", responses["4"])
	}
	if responses["5"].Error == nil || !strings.Contains(responses["5"].Error.Message, "exactly once") {
		t.Fatalf("duplicate initialize response: %+v", responses["5"])
	}
}

func TestMCPRejectsUnknownAndTrailingArguments(t *testing.T) {
	core := &stubCore{}
	responses := runMCP(t, core,
		`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recall_query","arguments":{"query":"x","soruces":["notes"]}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"not_a_tool","arguments":{}}}`,
		`{"id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall_sources","arguments":{"query":"should not be ignored"}}}`,
	)
	var badArgs callToolResult
	decodeResult(t, responses["1"], &badArgs)
	if !badArgs.IsError || !strings.Contains(badArgs.Content[0].Text, "unknown field") {
		t.Fatalf("misspelled narrowing field was not rejected: %+v", badArgs)
	}
	if responses["2"].Error == nil || responses["2"].Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool response: %+v", responses["2"])
	}
	if responses["3"].Error == nil || responses["3"].Error.Code != codeInvalidRequest {
		t.Fatalf("invalid JSON-RPC response: %+v", responses["3"])
	}
	var badSources callToolResult
	decodeResult(t, responses["4"], &badSources)
	if !badSources.IsError {
		t.Fatal("recall_sources ignored an unknown argument")
	}

	var args queryArgs
	if err := decodeArgs(json.RawMessage(`{"query":"x"} {"query":"y"}`), &args); err == nil {
		t.Fatal("decodeArgs accepted two JSON values")
	}
}

func TestMCPToolSchemasCompile(t *testing.T) {
	for _, tool := range toolSet() {
		for name, raw := range map[string]json.RawMessage{
			"input":  tool.InputSchema,
			"output": tool.OutputSchema,
		} {
			if len(raw) == 0 {
				continue
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("%s %s schema parse: %v", tool.Name, name, err)
			}
			compiler := jsonschema.NewCompiler()
			url := "mem://" + tool.Name + "/" + name
			if err := compiler.AddResource(url, doc); err != nil {
				t.Fatalf("%s %s schema add: %v", tool.Name, name, err)
			}
			if _, err := compiler.Compile(url); err != nil {
				t.Fatalf("%s %s schema compile: %v", tool.Name, name, err)
			}
		}
	}
}

func runMCP(t *testing.T, core Core, messages ...string) map[string]mcpResponse {
	t.Helper()
	var input strings.Builder
	for _, message := range messages {
		input.WriteString(message)
		input.WriteByte('\n')
	}
	var output bytes.Buffer
	if err := ServeMCP(t.Context(), strings.NewReader(input.String()), &output, MCPOptions{Core: core}); err != nil {
		t.Fatalf("ServeMCP: %v", err)
	}

	got := map[string]mcpResponse{}
	wantResponses := 0
	for _, message := range messages {
		var request mcpRequest
		if err := json.Unmarshal([]byte(message), &request); err == nil && !request.isNotification() {
			wantResponses++
		}
	}
	scan := bufio.NewScanner(&output)
	for scan.Scan() {
		var response mcpResponse
		if err := json.Unmarshal(scan.Bytes(), &response); err != nil {
			t.Fatalf("decode MCP response: %v\n%s", err, scan.Text())
		}
		got[string(response.ID)] = response
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != wantResponses {
		t.Fatalf("got %d responses for %d requests\n%s", len(got), wantResponses, output.String())
	}
	return got
}

func decodeResult(t *testing.T, response mcpResponse, out any) {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("MCP error: %+v", response.Error)
	}
	raw, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode result: %v\n%s", err, raw)
	}
}

func responseMap(t *testing.T, raw []byte) map[string]mcpResponse {
	t.Helper()
	got := map[string]mcpResponse{}
	scan := bufio.NewScanner(bytes.NewReader(raw))
	for scan.Scan() {
		var response mcpResponse
		if err := json.Unmarshal(scan.Bytes(), &response); err != nil {
			t.Fatalf("decode MCP response: %v\n%s", err, scan.Text())
		}
		got[string(response.ID)] = response
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// A tool result is not the response: the model receives the structured content,
// the text projection of it, and the JSON-RPC frame around both. Pricing only
// the structure left the text uncharged on the one surface whose entire
// audience is a context window, so this measures the bytes the client reads.
func TestMCPToolResultFitsTheBudget(t *testing.T) {
	for _, budget := range []int{600, 1500, 4000, recall.DefaultResponseTokens} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			args := fmt.Sprintf(`{"query":"decision","budget_tokens":%d}`, budget)
			if budget == recall.DefaultResponseTokens {
				// The unset case: the ceiling has to hold without being asked
				// for, because that is the case an agent will actually hit.
				args = `{"query":"decision"}`
			}
			var output bytes.Buffer
			err := ServeMCP(t.Context(), strings.NewReader(strings.Join([]string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_query","arguments":` + args + `}}`,
				"",
			}, "\n")), &output, MCPOptions{Core: &liveCore{}})
			if err != nil {
				t.Fatalf("ServeMCP: %v", err)
			}

			var result []byte
			for line := range bytes.SplitSeq(bytes.TrimRight(output.Bytes(), "\n"), []byte("\n")) {
				if bytes.Contains(line, []byte(`"id":2`)) {
					result = line
				}
			}
			if result == nil {
				t.Fatalf("no tool result in\n%s", output.String())
			}
			if got := evidence.EstimateTokens(string(result)); got > budget {
				t.Errorf("the tool result the client reads is %d tokens against a budget of %d", got, budget)
			}
		})
	}
}

// The tool result carries the same claim twice — once as text a model reads and
// once as structure — and the two must agree about what corroborates. A record
// reached through two source instances is one record: it has two members and
// one independent unit, and the text follows the unit.
func TestMCPFusedResultDoesNotClaimCorroboration(t *testing.T) {
	core := &stubCore{query: recall.QueryResponse{
		Outcome:  recall.OutcomeAnswered,
		Coverage: recall.CoverageComplete,
		Results: []recall.Result{{
			Primary: recall.Candidate{Locator: recall.Locator{SourceID: "projects-attention", Local: "p-1"}},
			Members: []recall.ClusterMember{
				{LineageRoot: "uid-attention:p-1"},
				{LineageRoot: "uid-projects:p-1"},
			},
			Explanation: recall.Explanation{
				SourceID:      "projects-attention",
				LineageRoot:   "uid-attention:p-1",
				Corroboration: recall.CorroborationExplanation{IndependentUnits: 1},
			},
		}},
		Suppressed: []recall.Suppression{{
			Reason:      recall.SuppressDuplicateView,
			Count:       1,
			LineageRoot: "uid-projects:p-1",
			FusedInto:   "uid-attention:p-1",
		}},
	}}
	responses := runMCP(t, core,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall_query","arguments":{"query":"sidecar"}}}`,
	)
	var got callToolResult
	decodeResult(t, responses["2"], &got)

	text := got.Content[0].Text
	if strings.Contains(text, "corroborated_by") {
		t.Errorf("one record read twice claims corroboration in the tool text:\n%s", text)
	}
	if !strings.Contains(text, "Withheld 1 result(s): duplicate_view") {
		t.Errorf("the withheld view is not reported to the model:\n%s", text)
	}
}
