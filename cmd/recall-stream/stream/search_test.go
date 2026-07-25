package stream_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

func search(t *testing.T, req recall.SearchRequest, corpusBody string) recall.SearchResponse {
	t.Helper()
	a, _ := start(t, fixture(t, corpusBody), settings(nil))
	req.Deadline = soon()
	if req.Limit == 0 {
		req.Limit = 10
	}
	resp, err := a.Search(context.Background(), req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return resp
}

func TestSchemaVersionAwareParsing(t *testing.T) {
	// The two versions spell the body differently. Reading a v2 line with the
	// v1 parser would lose its text silently, which is the whole reason the
	// version is parsed before the record and carried in the locator.
	resp := search(t, recall.SearchRequest{Query: "transcripts adapter"}, corpus)

	byID := map[string]recall.Candidate{}
	for _, c := range resp.Candidates {
		byID[c.SourceRecordID] = c
	}
	if got := byID["sig-0001"].Excerpt; !strings.Contains(got, "reference stream adapter") {
		t.Errorf("v1 record excerpt = %q, want its `body`", got)
	}
	if got := byID["sig-0003"].Excerpt; !strings.Contains(got, "Reviewer asked for transcripts") {
		t.Errorf("v2 record excerpt = %q, want its `text`", got)
	}
	if got := byID["sig-0003"].Locator.Local; got != "v2/sig-0003" {
		t.Errorf("locator local = %q, want the schema version in it", got)
	}
	if got := byID["sig-0001"].Locator.Local; got != "v1/sig-0001" {
		t.Errorf("locator local = %q, want v1/sig-0001", got)
	}
}

func TestExactIdentifierPartitionsAboveLexical(t *testing.T) {
	// exact_identifier is emitted only for a whole-token match on a stable
	// identifier. It sorts above everything else, mirroring the core's own
	// exact-match promotion, and is never a score bonus.
	resp := search(t, recall.SearchRequest{Query: "td-f62256 interesting"}, corpus)

	if len(resp.Candidates) < 2 {
		t.Fatalf("want at least two candidates, got %d", len(resp.Candidates))
	}
	first := resp.Candidates[0]
	if !first.Exact() || first.SourceRecordID != "sig-0001" {
		t.Fatalf("first candidate is %q with signals %v, want the exact identifier hit",
			first.SourceRecordID, first.MatchSignals)
	}
	for _, c := range resp.Candidates[1:] {
		if c.Exact() {
			t.Errorf("candidate %q at rank %d claims exact_identifier below a non-exact hit",
				c.SourceRecordID, c.LocalRank)
		}
	}
	for i, c := range resp.Candidates {
		if c.LocalRank != i+1 {
			t.Errorf("candidate %d carries local_rank %d; rank must be one-based and dense", i, c.LocalRank)
		}
	}
}

func TestSubstringOfAnIdentifierIsNotAnExactMatch(t *testing.T) {
	// "f62256" appears inside the id but is not the id. An unbounded substring
	// match must never carry exact_identifier.
	resp := search(t, recall.SearchRequest{Query: "f62256"}, corpus)
	for _, c := range resp.Candidates {
		if c.Exact() {
			t.Errorf("candidate %q claims exact_identifier for a substring", c.SourceRecordID)
		}
	}
}

func TestDerivedFromNamesTheUpstreamRecordExactly(t *testing.T) {
	// The point of this adapter. A signal projecting task td-f62256 must
	// declare the locator the Tasks adapter itself writes for that task, or
	// the two do not collapse into one lineage root and the projection
	// corroborates the original it is a copy of.
	resp := search(t, recall.SearchRequest{Query: "td-f62256 recall review"}, corpus)

	edges := map[string][]string{}
	for _, c := range resp.Candidates {
		for _, edge := range c.DerivedFrom {
			edges[c.SourceRecordID] = append(edges[c.SourceRecordID], edge.String())
		}
	}
	tests := []struct {
		record string
		want   []string
		why    string
	}{
		{
			record: "sig-0001",
			want:   []string{"tasks:td-f62256"},
			why:    "a mapped system yields the upstream source's own locator, character for character",
		},
		{
			record: "sig-0003",
			want:   []string{"jira-work:REC-118"},
			why:    "the edge uses the configured source_id, not the system name in the record",
		},
		{
			record: "sig-0002",
			want:   nil,
			why:    "slack is unmapped; an invented source_id would resolve somewhere, and a wrong lineage root is worse than a missing one",
		},
	}
	for _, tc := range tests {
		t.Run(tc.record, func(t *testing.T) {
			got := edges[tc.record]
			if len(got) != len(tc.want) {
				t.Fatalf("edges = %v, want %v (%s)", got, tc.want, tc.why)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("edge %d = %q, want %q (%s)", i, got[i], tc.want[i], tc.why)
				}
			}
		})
	}
}

func TestTimeFiltersAndAsOf(t *testing.T) {
	// as_of is declared as filter, so it must actually restrict by the
	// event_time the source stores. A source never answers a historical
	// question from current state.
	at := func(s string) *time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return &parsed
	}
	tests := []struct {
		name string
		req  recall.SearchRequest
		want []string
	}{
		{
			name: "no bound",
			req:  recall.SearchRequest{Query: "recall review adapter"},
			want: []string{"sig-0001", "sig-0002", "sig-0003"},
		},
		{
			name: "as_of excludes later events",
			req:  recall.SearchRequest{Query: "recall review adapter", AsOf: at("2026-05-02T09:21:00Z")},
			want: []string{"sig-0001", "sig-0002"},
		},
		{
			name: "since and until bound a window",
			req: recall.SearchRequest{Query: "", Filters: recall.Filters{
				Since: at("2026-05-02T09:15:00Z"), Until: at("2026-05-02T09:22:00Z"),
			}},
			want: []string{"sig-0002"},
		},
		{
			name: "record type filter",
			req:  recall.SearchRequest{Query: "", Filters: recall.Filters{RecordTypes: []recall.RecordType{recall.RecordMessage}}},
			want: []string{"sig-0002"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := search(t, tc.req, corpus)
			got := make([]string, 0, len(resp.Candidates))
			for _, c := range resp.Candidates {
				got = append(got, c.SourceRecordID)
			}
			if strings.Join(sorted(got), ",") != strings.Join(tc.want, ",") {
				t.Errorf("records = %v, want %v", sorted(got), tc.want)
			}
		})
	}
}

func TestIncrementalIngestionByByteOffset(t *testing.T) {
	// An append-only file only ever grows, so catching up costs the bytes
	// appended and nothing more. The partial line is the case that matters:
	// consuming a half-written record would index nonsense and never revisit
	// it, so the cursor stops at the last newline.
	dir := fixture(t, corpus)
	a, _ := start(t, dir, settings(nil))
	ctx := context.Background()
	req := recall.SearchRequest{Query: "", Limit: 50, Deadline: soon()}

	first, err := a.Search(ctx, req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(first.Candidates) != 3 {
		t.Fatalf("indexed %d records, want 3", len(first.Candidates))
	}

	appended := `{"schema":2,"id":"sig-0004","kind":"event","event_time":"2026-05-02T09:40:00Z","observed_at":"2026-05-02T09:40:01Z","system":"tasks","ref":"td-f62256","title":"Task td-f62256 closed","text":"Done.","actor":"marcus"}`
	half := appended[:60]

	appendTo(t, dir, half)
	partial, err := a.Search(ctx, req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(partial.Candidates) != 3 {
		t.Fatalf("a half-written record was indexed: %d candidates", len(partial.Candidates))
	}

	appendTo(t, dir, appended[60:]+"\n")
	complete, err := a.Search(ctx, req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(complete.Candidates) != 4 {
		t.Fatalf("indexed %d records after the write completed, want 4", len(complete.Candidates))
	}
	// A new generation was published, and the watermark moved with it. An
	// identical watermark across a changed source would be a false statement
	// about freshness.
	if first.SourceWatermark == complete.SourceWatermark {
		t.Error("the watermark did not move when the stream grew")
	}
	if complete.Candidates[0].SourceRevision == first.Candidates[0].SourceRevision {
		t.Error("the source revision did not change when a new generation was published")
	}
}

func TestSearchRespectsTheSmallerOfLimitAndMaxCandidates(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		max   int
		want  int
	}{
		{name: "request limit is smaller", limit: 1, max: 50, want: 1},
		{name: "configured cap is smaller", limit: 50, max: 2, want: 2},
		{name: "no request limit falls back to the cap", limit: 0, max: 2, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := start(t, fixture(t, corpus), settings(map[string]any{"max_candidates": tc.max}))
			resp, err := a.Search(context.Background(), recall.SearchRequest{
				Query: "", Limit: tc.limit, Deadline: soon(),
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(resp.Candidates) != tc.want {
				t.Errorf("returned %d candidates, want %d", len(resp.Candidates), tc.want)
			}
		})
	}
}

func TestSearchNoticesACancelledContext(t *testing.T) {
	// The only thing an adapter owes a cancel: notice, return, do not answer.
	a, _ := start(t, fixture(t, corpus), settings(map[string]any{"debug_stall_ms": 30000}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := a.Search(ctx, recall.SearchRequest{Query: "td-f62256", Limit: 10, Deadline: soon()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("search returned %v, want a cancellation", err)
	}
	if resp.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %q; a cancelled search is never a success", resp.Outcome)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("search took %s to notice the cancellation", elapsed)
	}
}

func TestExpandLevelsWidenAndBudgetTruncates(t *testing.T) {
	a, _ := start(t, fixture(t, corpus), settings(nil))
	locator := recall.Locator{SourceID: "signals", Local: "v1/sig-0001"}
	read := func(detail recall.DetailLevel, budget int64) recall.ExpandResponse {
		t.Helper()
		resp, err := a.Expand(context.Background(), recall.ExpandRequest{
			Locator: locator, Detail: detail, Budget: budget, Deadline: soon(),
		})
		if err != nil {
			t.Fatalf("expand %s: %v", detail, err)
		}
		return resp
	}

	summary := read(recall.DetailSummary, 4096)
	excerpt := read(recall.DetailExcerpt, 4096)
	full := read(recall.DetailFull, 4096)
	withContext := read(recall.DetailContext, 4096)

	// Each level starts with the previous one's output, so a caller comparing
	// two expansions sees added lines rather than rewritten ones.
	levels := []recall.ExpandResponse{summary, excerpt, full, withContext}
	for i := 1; i < len(levels); i++ {
		if !strings.HasPrefix(levels[i].Content, levels[i-1].Content) {
			t.Errorf("level %d does not extend level %d:\n%q\n%q",
				i, i-1, levels[i-1].Content, levels[i].Content)
		}
	}
	// Context groups adjacent events into an episode while keeping each
	// neighbour's own locator, so a reader can still expand one of them.
	if !strings.Contains(withContext.Content, "v2/sig-0003") {
		t.Errorf("context expansion carries no neighbour locators:\n%s", withContext.Content)
	}
	if summary.Provenance == "" || summary.SourceRevision == "" {
		t.Error("expansion carries no provenance or source revision")
	}

	cut := read(recall.DetailFull, 64)
	switch {
	case !cut.Truncated:
		t.Error("a 64-byte budget did not truncate")
	case cut.TruncationBoundary != "budget_bytes":
		t.Errorf("truncation boundary = %q, want budget_bytes", cut.TruncationBoundary)
	case int64(len(cut.Content)) > 64:
		t.Errorf("truncated content is %d bytes, over the 64-byte budget", len(cut.Content))
	}
}

func TestExpandFailsExplicitlyRatherThanSubstituting(t *testing.T) {
	// Expansion never returns a different revision or a nearby record. The two
	// failures are different facts: locator_unknown is about the reference,
	// locator_expired is about the source having changed under it.
	tests := []struct {
		name  string
		local string
		want  *protocol.Error
		why   string
	}{
		{
			name:  "not a stream locator",
			local: "sig-0001",
			want:  protocol.ErrLocatorUnknown,
			why:   "no schema segment, so this adapter cannot read the reference at all",
		},
		{
			name:  "schema the record no longer has",
			local: "v1/sig-0003",
			want:  protocol.ErrLocatorExpired,
			why:   "sig-0003 is v2; returning it would be the different revision the protocol forbids",
		},
		{
			name:  "record this generation does not hold",
			local: "v1/sig-9999",
			want:  protocol.ErrLocatorExpired,
			why:   "an append-only stream does not lose records, so a missing one means a rewrite",
		},
	}
	a, _ := start(t, fixture(t, corpus), settings(nil))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Expand(context.Background(), recall.ExpandRequest{
				Locator: recall.Locator{SourceID: "signals", Local: tc.local},
				Detail:  recall.DetailFull, Budget: 4096, Deadline: soon(),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v (%s)", err, tc.want, tc.why)
			}
		})
	}
}

func appendTo(t *testing.T, dir, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "signals.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
