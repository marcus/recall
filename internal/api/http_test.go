package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

type stubCore struct {
	profile string

	query       recall.QueryResponse
	queryErr    error
	queryFunc   func(context.Context, recall.QueryRequest) (recall.QueryResponse, error)
	expand      recall.ExpandResponse
	expandErr   error
	refresh     recall.RefreshResponse
	refreshErr  error
	sources     Listing
	sourcesErr  error
	doctor      Listing
	doctorErr   error
	lastQuery   recall.QueryRequest
	lastExpand  recall.ExpandRequest
	lastRefresh recall.RefreshRequest
}

func (s *stubCore) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	s.lastQuery = req
	if s.queryFunc != nil {
		return s.queryFunc(ctx, req)
	}
	return s.query, s.queryErr
}

func (s *stubCore) Expand(_ context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	s.lastExpand = req
	return s.expand, s.expandErr
}

func (s *stubCore) Refresh(_ context.Context, req recall.RefreshRequest) (recall.RefreshResponse, error) {
	s.lastRefresh = req
	return s.refresh, s.refreshErr
}

func (s *stubCore) Sources(context.Context) (Listing, error) { return s.sources, s.sourcesErr }
func (s *stubCore) Doctor(context.Context) (Listing, error)  { return s.doctor, s.doctorErr }
func (s *stubCore) Profile() string {
	if s.profile == "" {
		return "work"
	}
	return s.profile
}

func sampleQueryResponse() recall.QueryResponse {
	return recall.QueryResponse{
		Results: []recall.Result{{
			Primary: recall.Candidate{
				CandidateID:    "c-1",
				SourceUID:      "01SOURCE",
				SourceID:       "notes",
				SourceRecordID: "record-1",
				Locator:        recall.Locator{SourceID: "notes", SourceUID: "01SOURCE", Local: "record-1#L1-2"},
				RecordType:     recall.RecordDocument,
				Title:          "A decision",
				Excerpt:        "The typed payload survives.",
				LocalRank:      1,
			},
			Score: 0.5,
		}},
		SourceOutcomes: []recall.SourceReport{{
			SourceUID:  "01SOURCE",
			SourceID:   "notes",
			Outcome:    recall.SearchSuccess,
			Candidates: 1,
		}},
		Plan:     recall.Plan{Profile: "work", Limit: 10},
		Outcome:  recall.OutcomeAnswered,
		Coverage: recall.CoverageComplete,
		Elapsed:  12 * time.Millisecond,
	}
}

func TestHTTPClientPreservesTypedSurfaceResults(t *testing.T) {
	wantQuery := sampleQueryResponse()
	wantExpand := recall.ExpandResponse{
		Content: "evidence", Provenance: "notes.md#L1-2", SourceRevision: "abc123",
	}
	wantRefresh := recall.RefreshResponse{
		Outcome: recall.RefreshDegraded,
		Sources: []recall.RefreshSourceOutcome{{
			SourceID: "notes", Status: recall.RefreshSourceDegraded, Reason: recall.RefreshUnhealthy,
			Health: &recall.Health{Status: recall.HealthDegraded, Coverage: recall.IndexPartial},
		}},
	}
	wantSources := map[string]any{
		"profile": "work",
		"sources": []any{map[string]any{"source_id": "notes", "health": "healthy"}},
	}
	core := &stubCore{
		query:   wantQuery,
		expand:  wantExpand,
		refresh: wantRefresh,
		sources: Listing{Payload: wantSources, Status: StatusOK},
		doctor:  Listing{Payload: map[string]any{"status": "degraded"}, Status: StatusDegraded},
	}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
	defer server.Close()
	client := NewClient(ClientOptions{BaseURL: server.URL, Profile: "work"})

	gotQuery, err := client.Query(t.Context(), recall.QueryRequest{Query: "decision"})
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotQuery, wantQuery)
	if core.lastQuery.Profile != "work" || core.lastQuery.Mode != recall.ModeExplicit {
		t.Fatalf("server did not normalize query: %+v", core.lastQuery)
	}

	loc := recall.Locator{SourceID: "notes", Local: "record-1#L1-2"}
	gotExpand, err := client.Expand(t.Context(), recall.ExpandRequest{Locator: loc})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotExpand, wantExpand) {
		t.Fatalf("expand mismatch\n got: %#v\nwant: %#v", gotExpand, wantExpand)
	}
	if core.lastExpand.Detail != recall.DetailExcerpt {
		t.Fatalf("server did not apply expand default: %+v", core.lastExpand)
	}

	gotRefresh, err := client.Refresh(t.Context(), recall.RefreshRequest{SourceID: " notes ", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotRefresh, wantRefresh)
	if core.lastRefresh.SourceID != "notes" || !core.lastRefresh.Full || core.lastRefresh.Profile != "work" {
		t.Fatalf("server did not normalize refresh: %+v", core.lastRefresh)
	}

	gotSources, err := client.Sources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotSources.Status != StatusOK {
		t.Fatalf("sources status = %q", gotSources.Status)
	}
	assertSameJSON(t, gotSources.Payload, wantSources)

	gotDoctor, err := client.Doctor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gotDoctor.Status != StatusDegraded {
		t.Fatalf("doctor status = %q", gotDoctor.Status)
	}
	assertSameJSON(t, gotDoctor.Payload, core.doctor.Payload)
}

func TestRefreshHTTPIsAuthenticatedStrictAndKeepsFailedResultTyped(t *testing.T) {
	want := recall.RefreshResponse{
		Outcome: recall.RefreshFailed,
		Sources: []recall.RefreshSourceOutcome{{
			SourceID: "docs", Status: recall.RefreshSourceFailed, Reason: recall.RefreshOperationFailed,
		}},
	}
	server := httptest.NewServer(NewHandler(ServerOptions{
		Core: &stubCore{refresh: want}, BearerToken: "secret",
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/refresh",
		strings.NewReader(`{"source_id":"docs"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated refresh status = %d", resp.StatusCode)
	}

	client := NewClient(ClientOptions{BaseURL: server.URL, BearerToken: "secret"})
	got, err := client.Refresh(t.Context(), recall.RefreshRequest{SourceID: "docs"})
	if err != nil {
		t.Fatalf("semantic failed result became transport error: %v", err)
	}
	assertSameJSON(t, got, want)

	req, err = http.NewRequest(http.MethodPost, server.URL+"/v1/refresh",
		strings.NewReader(`{"source_id":"docs","surprise":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown refresh field status = %d, want 400", resp.StatusCode)
	}
}

func TestRefreshHTTPMapsSemanticOutcomesWithoutLosingTypedBody(t *testing.T) {
	for _, tc := range []struct {
		outcome recall.RefreshOutcome
		status  int
	}{
		{recall.RefreshSucceeded, http.StatusOK},
		{recall.RefreshDegraded, http.StatusPartialContent},
		{recall.RefreshFailed, http.StatusServiceUnavailable},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			want := recall.RefreshResponse{Outcome: tc.outcome, Sources: []recall.RefreshSourceOutcome{}}
			server := httptest.NewServer(NewHandler(ServerOptions{Core: &stubCore{refresh: want}}))
			defer server.Close()
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/refresh", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status || resp.Header.Get(HeaderOutcome) != string(tc.outcome) {
				t.Fatalf("status/header = %d/%q, want %d/%q",
					resp.StatusCode, resp.Header.Get(HeaderOutcome), tc.status, tc.outcome)
			}
			var got recall.RefreshResponse
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			assertSameJSON(t, got, want)
		})
	}
}

func TestFailedQueryIsTypedResultNotTransportError(t *testing.T) {
	want := recall.QueryResponse{
		Outcome: recall.OutcomeFailed, Coverage: recall.CoverageDegraded,
		SourceOutcomes: []recall.SourceReport{{
			SourceID: "notes", Outcome: recall.SearchUnavailable, Reason: "unreachable",
		}},
	}
	core := &stubCore{query: want}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
	defer server.Close()

	got, err := NewClient(ClientOptions{BaseURL: server.URL}).Query(t.Context(), recall.QueryRequest{Query: "x"})
	if err != nil {
		t.Fatalf("failed outcome became client error: %v", err)
	}
	assertSameJSON(t, got, want)
}

func TestClientRefusesAListingFromTheWrongProfile(t *testing.T) {
	core := &stubCore{
		profile: "personal",
		sources: Listing{Payload: map[string]any{"profile": "personal"}, Status: StatusOK},
	}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
	defer server.Close()

	_, err := NewClient(ClientOptions{BaseURL: server.URL, Profile: "work"}).Sources(t.Context())
	var problem Problem
	if !errors.As(err, &problem) || problem.Code != CodeProfileMismatch {
		t.Fatalf("profile mismatch error = %#v, want %s problem", err, CodeProfileMismatch)
	}
}

func TestHTTPBearerAndBrowserGuards(t *testing.T) {
	core := &stubCore{sources: Listing{Payload: map[string]any{"sources": []any{}}, Status: StatusOK}}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core, BearerToken: "correct horse"}))
	defer server.Close()

	for _, tc := range []struct {
		name   string
		token  string
		origin string
		status int
		code   string
	}{
		{"missing token", "", "", http.StatusUnauthorized, CodeUnauthorized},
		{"wrong token", "wrong", "", http.StatusUnauthorized, CodeUnauthorized},
		{"right token", "correct horse", "", http.StatusOK, ""},
		{"browser still refused", "correct horse", "https://evil.example", http.StatusForbidden, CodeOriginRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+"/v1/sources", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.code != "" {
				var body struct {
					Error Problem `json:"error"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != tc.code {
					t.Fatalf("problem code = %q, want %q", body.Error.Code, tc.code)
				}
			}
		})
	}

	client := NewClient(ClientOptions{BaseURL: server.URL, BearerToken: "correct horse"})
	if _, err := client.Sources(t.Context()); err != nil {
		t.Fatalf("authenticated client: %v", err)
	}
}

func TestExpandErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"malformed", recall.ErrMalformedLocator, http.StatusBadRequest, CodeMalformedLocator},
		{"denied", protocol.ErrSourceDenied, http.StatusForbidden, CodeDenied},
		{"not configured", protocol.ErrSourceNotConfigured, http.StatusNotFound, CodeNotConfigured},
		{"unknown", protocol.ErrLocatorUnknown, http.StatusNotFound, CodeUnknownLocator},
		{"expired", protocol.ErrLocatorExpired, http.StatusGone, CodeExpiredLocator},
		{"unreachable", protocol.ErrSourceUnavailable, http.StatusBadGateway, CodeUnreachable},
		{"budget", protocol.ErrBudgetExceeded, http.StatusBadGateway, CodeBudgetExceeded},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, CodeTimeout},
		{"cancelled", context.Canceled, http.StatusGatewayTimeout, CodeTimeout},
		{"other", errors.New("adapter prose must not escape"), http.StatusBadGateway, CodeFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			core := &stubCore{expandErr: tc.err}
			handler := NewHandler(ServerOptions{Core: core})
			body := `{"locator":"notes:record"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/expand", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
			var got struct {
				Error Problem `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Error.Code, tc.code)
			}
			if strings.Contains(got.Error.Message, "adapter prose") {
				t.Fatal("adapter error text escaped into transport")
			}
		})
	}
}

// Every product surface bounds its own answer. The core leaves an unset budget
// unbounded because a library caller holding the struct pays no rendering cost;
// a caller across a transport has a context window instead, and one unbounded
// query once cost 203 KB of it.
func TestServerBoundsAnUnsetResponseBudget(t *testing.T) {
	core := &stubCore{query: recall.QueryResponse{
		Outcome: recall.OutcomeAbstained, Coverage: recall.CoverageComplete,
	}}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
	defer server.Close()
	client := NewClient(ClientOptions{BaseURL: server.URL, Profile: "work"})

	if _, err := client.Query(t.Context(), recall.QueryRequest{Query: "decision"}); err != nil {
		t.Fatal(err)
	}
	if got := core.lastQuery.Budget.ResponseTokens; got != recall.DefaultResponseTokens {
		t.Errorf("an unset budget reached the core as %d, want the default ceiling %d",
			got, recall.DefaultResponseTokens)
	}

	// Unbounded stays something a caller can ask for outright, which is what
	// makes the ceiling a default rather than a limit.
	unbounded := recall.QueryRequest{Query: "decision", Budget: recall.Budget{ResponseTokens: -1}}
	if _, err := client.Query(t.Context(), unbounded); err != nil {
		t.Fatal(err)
	}
	if got := core.lastQuery.Budget.ResponseTokens; got != -1 {
		t.Errorf("an explicit unbounded budget reached the core as %d", got)
	}

	// An undeclared surface is the bytes this transport sends.
	if got := core.lastQuery.Budget.Surface; got != recall.SurfaceStructured {
		t.Errorf("an undeclared surface reached the core as %q, want the wire form", got)
	}

	// A declared projection is honored: the client renders pointers from this
	// body, and pricing it as the body would make the same query answer
	// differently in process and over a socket.
	pointer := recall.QueryRequest{Query: "decision", Budget: recall.Budget{ResponseTokens: 500, Surface: recall.SurfacePointer}}
	if _, err := client.Query(t.Context(), pointer); err != nil {
		t.Fatal(err)
	}
	if got := core.lastQuery.Budget.Surface; got != recall.SurfacePointer {
		t.Errorf("a declared projection reached the core as %q", got)
	}
}

// A surface outside the vocabulary is refused rather than repaired, and `tool`
// is outside it here: a tool result is consumed where it is produced, and this
// server is not producing one.
func TestHTTPRefusesASurfaceItCannotPrice(t *testing.T) {
	for _, surface := range []recall.ResponseSurface{recall.SurfaceTool, "cheap"} {
		t.Run(string(surface), func(t *testing.T) {
			core := &stubCore{query: recall.QueryResponse{Outcome: recall.OutcomeAbstained}}
			server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
			defer server.Close()

			resp := post(t, server.URL+"/v1/query", recall.QueryRequest{
				Query:  "decision",
				Budget: recall.Budget{ResponseTokens: 500, Surface: surface},
			})
			if resp.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", resp.status, resp.body)
			}
			if !strings.Contains(resp.body, "budget surface") {
				t.Errorf("the refusal does not say what was wrong with it: %s", resp.body)
			}
		})
	}
}

// The budget bounds what the declaring caller consumes. Undeclared, that is the
// body this transport sends, measured in the bytes it sends; declared, it is
// the projection the client renders, measured the way that projection is
// priced. Both are asserted here because the second is what restores parity
// between `recall query` and `recall query --server`, and the first is what
// stops an undeclared caller being handed an unbounded body.
func TestHTTPBudgetBoundsWhatTheCallerConsumes(t *testing.T) {
	const budget = 900

	for _, tc := range []struct {
		name    string
		surface recall.ResponseSurface
	}{
		{"undeclared", ""},
		{"structured", recall.SurfaceStructured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core := &liveCore{}
			server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
			defer server.Close()

			got := post(t, server.URL+"/v1/query", recall.QueryRequest{
				Query:  "decision",
				Budget: recall.Budget{ResponseTokens: budget, Surface: tc.surface},
			})
			if tokens := evidence.EstimateTokens(got.body); tokens > budget {
				t.Errorf("the server sent %d tokens against a budget of %d", tokens, budget)
			}
		})
	}

	t.Run("declared projection", func(t *testing.T) {
		core := &liveCore{}
		server := httptest.NewServer(NewHandler(ServerOptions{Core: core}))
		defer server.Close()

		body := recall.QueryRequest{
			Query:  "decision",
			Budget: recall.Budget{ResponseTokens: budget, Surface: recall.SurfacePointer},
		}
		resp := decodeResponse(t, post(t, server.URL+"/v1/query", body).body)

		// Re-priced as the projection the caller said it would render. That is
		// the number the budget named; the body it travelled in is larger, and
		// deliberately so.
		priced := projected{}.Frame(resp)
		for i, r := range resp.Results {
			priced += projected{}.Result(i+1, r)
		}
		if priced > budget {
			t.Errorf("the declared projection costs %d tokens against a budget of %d", priced, budget)
		}

		body.Budget.Surface = recall.SurfaceStructured
		wire := decodeResponse(t, post(t, server.URL+"/v1/query", body).body)
		if len(resp.Results) <= len(wire.Results) {
			t.Errorf("the declaration changed nothing: %d results as a projection, %d as the body",
				len(resp.Results), len(wire.Results))
		}
	})
}

// projected stands in for a client-side renderer: a pointer tier costs a
// fraction of the body it is read from. The CLI's own model is asserted against
// its renderer in internal/cli; what matters here is that the declaration
// reaches the shaper.
type projected struct{}

func (projected) Frame(recall.QueryResponse) int { return 20 }

func (projected) Result(_ int, r recall.Result) int {
	return evidence.EstimateTokens(r.Primary.Locator.String()) +
		evidence.EstimateTokens(r.Primary.Title) + evidence.EstimateTokens(r.Primary.Excerpt) + 4
}

func decodeResponse(t *testing.T, body string) recall.QueryResponse {
	t.Helper()
	var resp recall.QueryResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decoding the response: %v\n%s", err, body)
	}
	return resp
}

type posted struct {
	status int
	body   string
}

func post(t *testing.T, url string, req recall.QueryRequest) posted {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sent, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return posted{status: resp.StatusCode, body: string(sent)}
}

// liveCore shapes its answer the way the application core does, so a transport
// test measures what a transport would really send.
type liveCore struct{ stubCore }

func (c *liveCore) Query(_ context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	c.lastQuery = req
	resp := recall.QueryResponse{
		Results:        manyResults(40),
		SourceOutcomes: []recall.SourceReport{{SourceID: "notes", Outcome: recall.SearchSuccess}},
		SourceSummary: &recall.SourceSummary{
			Sources:  1,
			Outcomes: map[recall.SearchOutcome]int{recall.SearchSuccess: 1},
		},
		Plan:     recall.Plan{Profile: "work", Limit: 20, Sources: []recall.PlanSource{{SourceID: "notes", Eligible: true}}},
		Outcome:  recall.OutcomeAnswered,
		Coverage: recall.CoverageComplete,
	}
	return evidence.Shape(resp, req.Budget, costFor(req.Budget.Surface)).Response, nil
}

// costFor is the transports' own registration, which is what a server process
// hands the core: the tool surface priced by the transport that renders it, the
// human tiers by the client that will, and everything else by its own
// serialization.
func costFor(surface recall.ResponseSurface) evidence.Cost {
	switch surface {
	case recall.SurfaceTool:
		return ToolCost{}
	case recall.SurfacePointer, recall.SurfaceExplained:
		return projected{}
	default:
		return evidence.StructuredCost{}
	}
}

func manyResults(n int) []recall.Result {
	out := make([]recall.Result, n)
	for i := range out {
		out[i] = recall.Result{
			Primary: recall.Candidate{
				CandidateID: fmt.Sprintf("doc-%03d", i),
				Locator:     recall.Locator{SourceID: "notes", Local: fmt.Sprintf("doc-%03d", i)},
				RecordType:  recall.RecordDocument,
				Title:       fmt.Sprintf("Document %03d", i),
				Excerpt:     strings.Repeat("evidence body text ", 10),
			},
			Explanation: recall.Explanation{SourceID: "notes", LocalRank: i + 1},
		}
	}
	return out
}

func TestHTTPCancellationReachesCore(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	core := &stubCore{queryFunc: func(ctx context.Context, _ recall.QueryRequest) (recall.QueryResponse, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return recall.QueryResponse{}, ctx.Err()
	}}
	server := httptest.NewServer(NewHandler(ServerOptions{Core: core, RequestTimeout: time.Minute}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := NewClient(ClientOptions{BaseURL: server.URL}).Query(ctx, recall.QueryRequest{Query: "slow"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request cancellation did not reach core")
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled client request returned no error")
	}
}

func assertSameJSON(t *testing.T, got, want any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gv, wv any
	if err := json.Unmarshal(g, &gv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w, &wv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", g, w)
	}
}
