package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

var testNow = time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC)

type fakeCall struct {
	Key     string
	Args    []string
	Operand string
}

type fakeRunner struct {
	answers map[string]any
	errs    map[string]error
	calls   []fakeCall
}

func (f *fakeRunner) Kind() string { return "fake" }
func (f *fakeRunner) Now() (time.Time, bool) {
	return testNow, true
}
func (f *fakeRunner) Run(ctx context.Context, key string, args []string, operand string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, fakeCall{Key: key, Args: append([]string(nil), args...), Operand: operand})
	if err := f.errs[key]; err != nil {
		return err
	}
	value, ok := f.answers[key]
	if !ok {
		return protocol.Errorf(protocol.CodeSourceUnavailable, "no fake response for %s", key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

var ordinaryThread = thread{
	ID: "thr_1", Date: "2026-07-25 14:02",
	From: "Dana <dana@example.test>", Subject: "Re: dentist referral",
	Labels: []string{"UNREAD", "INBOX", "CATEGORY_PERSONAL"}, MessageCount: 2,
}

var credentialThread = thread{
	ID: "thr_2", Date: "2026-07-25 05:16",
	From: "Example Login <login@example.test>", Subject: "Sign in to Example",
	Labels: []string{"INBOX", "CATEGORY_UPDATES"}, MessageCount: 1,
}

var bulkBodyOnly = thread{
	ID: "thr_bulk_body", Date: "2026-07-24 18:00",
	From: "Venue News <shows@example.test>", Subject: "Weekend listings",
	Labels: []string{"UNREAD", "IMPORTANT", "CATEGORY_PROMOTIONS"}, MessageCount: 1,
}

var bulkHeaderMatch = thread{
	ID: "thr_bulk_header", Date: "2026-07-24 17:00",
	From: "Bonnie Books <newsletter@example.test>", Subject: "Bonnie summer reading list",
	Labels: []string{"CATEGORY_PROMOTIONS"}, MessageCount: 1,
}

var nonBulkBodyOnly = thread{
	ID: "thr_body", Date: "2026-07-24 16:00",
	From: "SIXT <rental@example.test>", Subject: "Rental damage report",
	Labels: []string{"IMPORTANT", "CATEGORY_PERSONAL", "INBOX"}, MessageCount: 2,
}

func initialized(t *testing.T, runner Runner, settings map[string]any) *Adapter {
	t.Helper()
	a := New(Options{Runner: runner})
	_, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1,
		ProtocolVersionMax: 1,
		Workdir:            t.TempDir(),
		SourceID:           "mail",
		Location:           "owner@example.test",
		Settings:           settings,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return a
}

func runnerWithSearch(threads ...thread) *fakeRunner {
	return &fakeRunner{answers: map[string]any{
		"gmail-search": searchPayload{Threads: threads},
	}}
}

func search(t *testing.T, a *Adapter, query string) recall.SearchResponse {
	t.Helper()
	resp, err := a.Search(t.Context(), recall.SearchRequest{
		Query: query, Filters: recall.Filters{}, Limit: 10,
		Deadline: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return resp
}

func TestInitializeDeclaresFirstPartyGmailContract(t *testing.T) {
	a := New(Options{Runner: &fakeRunner{}})
	manifest, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		SourceID: "mail", Location: "owner@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.AdapterID != AdapterID || manifest.DisplayName != "Gmail" {
		t.Fatalf("manifest identity = %+v", manifest)
	}
	if !manifest.Supports(recall.FreshnessLive) || manifest.AsOfSupport != recall.AsOfNone {
		t.Fatalf("manifest freshness = %+v", manifest)
	}
	if manifest.RelevanceBasis != recall.RelevanceLexicalSpan {
		t.Fatalf("relevance_basis = %q, want lexical_span", manifest.RelevanceBasis)
	}
	if manifest.Sensitivity != recall.SensitivityConfidential {
		t.Fatalf("sensitivity = %s", manifest.Sensitivity)
	}
}

func TestInitializeAcceptsLegacyGoogleURIAndRejectsAmbiguousInput(t *testing.T) {
	for _, location := range []string{"owner@example.test", "google://owner@example.test"} {
		a := New(Options{Runner: &fakeRunner{}})
		if _, err := a.Initialize(t.Context(), adapter.Config{
			ProtocolVersionMin: 1, ProtocolVersionMax: 1,
			SourceID: "mail", Location: location,
		}); err != nil {
			t.Errorf("location %q: %v", location, err)
		}
	}
	a := New(Options{Runner: &fakeRunner{}})
	_, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		SourceID: "mail", Location: "/tmp/not-an-account",
	})
	if !errors.Is(err, protocol.ErrInvalidParams) {
		t.Fatalf("invalid location error = %v", err)
	}
}

func TestSettingsRejectUnknownAndWrongTypedValues(t *testing.T) {
	for _, settings := range []map[string]any{
		{"scope_qurey": "in:inbox"},
		{"max_candidates": "twenty"},
		{"newer_than_days": -1},
	} {
		a := New(Options{Runner: &fakeRunner{}})
		_, err := a.Initialize(t.Context(), adapter.Config{
			ProtocolVersionMin: 1, ProtocolVersionMax: 1,
			SourceID: "mail", Location: "owner@example.test", Settings: settings,
		})
		if !errors.Is(err, protocol.ErrInvalidParams) {
			t.Errorf("settings %#v error = %v", settings, err)
		}
	}
}

func TestSearchUsesNarrowDefaultCorpusWithoutDegradingIt(t *testing.T) {
	runner := runnerWithSearch(ordinaryThread)
	resp := search(t, initialized(t, runner, nil), "dentist")
	if resp.Outcome != recall.SearchSuccess {
		t.Fatalf("outcome = %s", resp.Outcome)
	}
	scope := resp.Diagnostics["scope_query"]
	if scope != defaultScope {
		t.Fatalf("scope = %q", scope)
	}
	if !strings.Contains(runner.calls[0].Operand, "-category:promotions") {
		t.Fatalf("gog query = %q", runner.calls[0].Operand)
	}
}

func TestSearchReportsBrowseRecencyAndPaginationBoundaries(t *testing.T) {
	runner := runnerWithSearch(ordinaryThread)
	runner.answers["gmail-search"] = searchPayload{
		Threads: []thread{ordinaryThread}, NextPageToken: "next",
	}
	a := initialized(t, runner, map[string]any{"newer_than_days": 30})
	resp := search(t, a, "")
	if resp.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %s", resp.Outcome)
	}
	reason := resp.Diagnostics["coverage_reason"].(string)
	for _, want := range []string{"empty query", "last 30 days", "more matches"} {
		if !strings.Contains(reason, want) {
			t.Errorf("coverage reason %q missing %q", reason, want)
		}
	}
	query := runner.calls[0].Operand
	for _, want := range []string{"in:inbox", "is:unread", "category:primary", "-from:me", "newer_than:14d"} {
		if !strings.Contains(query, want) {
			t.Errorf("browse query %q missing %q", query, want)
		}
	}
	if resp.Candidates[0].ConfirmedAt != nil {
		t.Error("partial search claimed complete confirmation")
	}
}

func TestSearchPreservesPointerSafetyAndRaisesCredentials(t *testing.T) {
	resp := search(t, initialized(t,
		runnerWithSearch(ordinaryThread, credentialThread), nil), "dentist")
	if strings.Contains(resp.Candidates[0].Excerpt, "http") {
		t.Fatalf("unsafe excerpt = %q", resp.Candidates[0].Excerpt)
	}
	if !strings.Contains(resp.Candidates[0].Excerpt, "Dana") ||
		!strings.Contains(resp.Candidates[0].Excerpt, "dentist referral") {
		t.Fatalf("excerpt = %q", resp.Candidates[0].Excerpt)
	}
	if resp.Candidates[1].Sensitivity != recall.SensitivityRestricted {
		t.Fatalf("credential sensitivity = %s", resp.Candidates[1].Sensitivity)
	}
}

func TestSearchStripsAndRestrictsSchemeLessLinksInSafeFields(t *testing.T) {
	linked := ordinaryThread
	linked.Subject = "Open portal.example/reset?token=secret"
	resp := search(t, initialized(t, runnerWithSearch(linked), nil), "open")
	got := resp.Candidates[0]
	if strings.Contains(got.Title, "portal.example") ||
		strings.Contains(got.Excerpt, "portal.example") ||
		strings.Contains(got.Metadata["subject"].(string), "portal.example") {
		t.Fatalf("pointer retained scheme-less link: %+v", got)
	}
	if !strings.Contains(got.Title, "[url removed]") ||
		got.Sensitivity != recall.SensitivityRestricted {
		t.Fatalf("pointer safety = %+v", got)
	}
}

func TestSearchDoesNotTreatDottedFilenamesAsBearerLinks(t *testing.T) {
	attachment := ordinaryThread
	attachment.Subject = "Please review invoice.pdf"
	resp := search(t, initialized(t, runnerWithSearch(attachment), nil), "invoice")
	got := resp.Candidates[0]
	if got.Title != attachment.Subject ||
		got.Sensitivity != recall.SensitivityConfidential {
		t.Fatalf("ordinary dotted filename was treated as a link: %+v", got)
	}
}

func TestHeaderRelevanceAndBulkNoisePolicy(t *testing.T) {
	tests := []struct {
		name          string
		thread        thread
		query         string
		wantRelevance *float64
		wantNil       bool
	}{
		{"direct header", bulkHeaderMatch, "bonnie", ptrFloat(0.5), false},
		{"bulk body only", bulkBodyOnly, "bonnie", ptrFloat(0), false},
		{"ordinary body only", nonBulkBodyOnly, "bonnie", nil, true},
		{"explicit bulk", bulkBodyOnly, "bonnie category:promotions", nil, true},
		{"mixed operator still suppresses", bulkBodyOnly, "bonnie after:2026/01/01", ptrFloat(0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := search(t, initialized(t, runnerWithSearch(tt.thread),
				map[string]any{"scope_query": "-in:spam -in:trash -in:chats"}), tt.query)
			got := resp.Candidates[0].Relevance
			if tt.wantNil {
				if got != nil {
					t.Fatalf("relevance = %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("relevance is nil")
			}
			if tt.wantRelevance != nil && *tt.wantRelevance == 0 && *got != 0 {
				t.Fatalf("relevance = %v, want 0", *got)
			}
			if tt.wantRelevance != nil && *tt.wantRelevance > 0 && *got <= 0.1 {
				t.Fatalf("relevance = %v, want measurable header match", *got)
			}
		})
	}
}

func TestExactThreadIDIsPromotedWithoutBulkPenalty(t *testing.T) {
	resp := search(t, initialized(t, runnerWithSearch(bulkBodyOnly, ordinaryThread),
		map[string]any{"scope_query": "-in:spam"}), "thr_bulk_body")
	got := resp.Candidates[0]
	if got.CandidateID != "thr_bulk_body" ||
		!reflect.DeepEqual(got.MatchSignals, []recall.MatchSignal{recall.MatchExactIdentifier}) {
		t.Fatalf("candidate = %+v", got)
	}
	if got.Relevance != nil {
		t.Fatalf("exact identifier relevance = %v, want nil", *got.Relevance)
	}
}

func TestOperatorSyntaxDoesNotDiluteHeaderRelevance(t *testing.T) {
	plain := search(t, initialized(t, runnerWithSearch(bulkHeaderMatch), nil), "bonnie").Candidates[0]
	mixed := search(t, initialized(t, runnerWithSearch(bulkHeaderMatch), nil),
		"bonnie after:2026/01/01 is:important").Candidates[0]
	if *plain.LocalScore != *mixed.LocalScore || *plain.Relevance != *mixed.Relevance {
		t.Fatalf("plain = (%v,%v), mixed = (%v,%v)",
			*plain.LocalScore, *plain.Relevance, *mixed.LocalScore, *mixed.Relevance)
	}
}

func TestSearchPassesDashLeadingQueryAsOperand(t *testing.T) {
	runner := runnerWithSearch(ordinaryThread)
	search(t, initialized(t, runner, nil), "-from:me refund")
	call := runner.calls[0]
	if !strings.Contains(call.Operand, "-from:me refund") {
		t.Fatalf("operand = %q", call.Operand)
	}
	for _, arg := range call.Args {
		if arg == "-from:me refund" {
			t.Fatal("query was passed as a flag")
		}
	}
}

func TestSearchMapsTimeFiltersToWidenedGmailDates(t *testing.T) {
	runner := runnerWithSearch(ordinaryThread)
	a := initialized(t, runner, nil)
	since := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	_, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "refund", Filters: recall.Filters{Since: &since, Until: &until}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	query := runner.calls[0].Operand
	if !strings.Contains(query, "after:2026/06/30") ||
		!strings.Contains(query, "before:2026/07/11") {
		t.Fatalf("query = %q", query)
	}
	resp, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "refund", Filters: recall.Filters{Since: &since, Until: &until}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 0 {
		t.Fatalf("post-filter retained out-of-window mail: %+v", resp.Candidates)
	}
}

func TestSearchRetainsMinuteResolutionBoundaryMatchesAsPartial(t *testing.T) {
	boundary := ordinaryThread
	boundary.Date = "2026-07-01 12:00"
	a := initialized(t, runnerWithSearch(boundary), nil)
	since := time.Date(2026, 7, 1, 12, 0, 30, 0, time.UTC)
	resp, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "refund", Filters: recall.Filters{Since: &since}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("boundary-minute candidate was dropped: %+v", resp.Candidates)
	}
	if resp.Outcome != recall.SearchPartial ||
		!strings.Contains(resp.Diagnostics["coverage_reason"].(string), "retained conservatively") {
		t.Fatalf("boundary response = %+v", resp)
	}

	outside := boundary
	outside.Date = "2026-07-01 11:59"
	a = initialized(t, runnerWithSearch(outside), nil)
	resp, err = a.Search(t.Context(), recall.SearchRequest{
		Query: "refund", Filters: recall.Filters{Since: &since}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Candidates) != 0 || resp.Outcome != recall.SearchSuccess {
		t.Fatalf("definitely out-of-window response = %+v", resp)
	}
}

func TestUnsupportedFiltersAndRecordTypesSkipHonestly(t *testing.T) {
	a := initialized(t, runnerWithSearch(), nil)
	project, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "x", Filters: recall.Filters{Project: "recall"},
	})
	if err != nil || project.Outcome != recall.SearchSkipped ||
		project.Reason != recall.SkipFilterUnsupported {
		t.Fatalf("project response = %+v, err = %v", project, err)
	}
	docs, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "x", Filters: recall.Filters{RecordTypes: []recall.RecordType{recall.RecordDocument}},
	})
	if err != nil || docs.Outcome != recall.SearchSkipped ||
		docs.Reason != recall.SkipRecordTypeMismatch {
		t.Fatalf("record response = %+v, err = %v", docs, err)
	}
}

func TestAsOfIsRefused(t *testing.T) {
	a := initialized(t, runnerWithSearch(), nil)
	asOf := testNow
	resp, err := a.Search(t.Context(), recall.SearchRequest{Query: "x", AsOf: &asOf})
	if !errors.Is(err, protocol.ErrAsOfUnsupported) || resp.Outcome != recall.SearchFailed {
		t.Fatalf("response = %+v, err = %v", resp, err)
	}
}

func TestHealthDistinguishesMissingCredentialsAndSuccess(t *testing.T) {
	denied := &fakeRunner{answers: map[string]any{
		"auth-list": authList{Accounts: []struct {
			Email string `json:"email"`
		}{{Email: "someone@example.test"}}},
	}}
	h, err := initialized(t, denied, nil).Health(t.Context())
	if err != nil || h.Status != recall.HealthDenied || h.Coverage != recall.IndexUnknown {
		t.Fatalf("denied health = %+v, err = %v", h, err)
	}

	healthy := &fakeRunner{answers: map[string]any{
		"auth-list":   authList{},
		"gmail-probe": searchPayload{Threads: []thread{ordinaryThread}},
	}}
	h, err = initialized(t, healthy, nil).Health(t.Context())
	if err != nil || h.Status != recall.HealthHealthy || h.Coverage != recall.IndexComplete {
		t.Fatalf("healthy health = %+v, err = %v", h, err)
	}
}

func TestHealthClassifiesGogCredentialFailureAsDenied(t *testing.T) {
	runner := &fakeRunner{
		answers: map[string]any{"auth-list": authList{}},
		errs: map[string]error{
			"gmail-probe": protocol.Errorf(protocol.CodeSourceDenied, "expired"),
		},
	}
	h, err := initialized(t, runner, nil).Health(t.Context())
	if err != nil || h.Status != recall.HealthDenied {
		t.Fatalf("health = %+v, err = %v", h, err)
	}
}

func ptrFloat(value float64) *float64 { return &value }
