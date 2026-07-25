package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

type stubCore struct {
	profile string

	query      recall.QueryResponse
	queryErr   error
	queryFunc  func(context.Context, recall.QueryRequest) (recall.QueryResponse, error)
	expand     recall.ExpandResponse
	expandErr  error
	sources    Listing
	sourcesErr error
	doctor     Listing
	doctorErr  error
	lastQuery  recall.QueryRequest
	lastExpand recall.ExpandRequest
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
	wantSources := map[string]any{
		"profile": "work",
		"sources": []any{map[string]any{"source_id": "notes", "health": "healthy"}},
	}
	core := &stubCore{
		query:   wantQuery,
		expand:  wantExpand,
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
