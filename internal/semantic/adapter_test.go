package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/semantic/antfly"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

func TestRefreshPublishesAndReusesVectors(t *testing.T) {
	ctx := context.Background()
	a, fake, root := fixture(t)
	writeDoc(t, root, "one.md", "# One\n\norbital id-123\n")
	writeDoc(t, root, "two.html", "<h1>Two</h1>\n<p>orbital pool</p><p data-noindex>excluded</p>\n")
	h, err := a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || h.Status != recall.HealthHealthy || h.IndexedCount != 2 || fake.embedded != 2 {
		t.Fatalf("first refresh: health=%+v, embedded=%d, err=%v", h, fake.embedded, err)
	}
	first := h.IndexGeneration
	h, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || h.IndexGeneration != first || fake.embedded != 2 {
		t.Fatalf("unchanged refresh: health=%+v, embedded=%d, err=%v", h, fake.embedded, err)
	}

	hits := search(t, a, "id-123")
	if len(hits.Candidates) != 1 || hits.Candidates[0].SourceRecordID != "one.md" {
		t.Fatalf("named identifier: %+v", hits)
	}
	hits = search(t, a, "id-124")
	if len(hits.Candidates) != 0 || hits.Outcome != recall.SearchSuccess {
		t.Fatalf("absent identifier: %+v", hits)
	}
	exp, err := a.Expand(ctx, recall.ExpandRequest{
		Locator: recall.Locator{Local: "two.html#L1-L2"}, Detail: recall.DetailFull,
	})
	if err != nil || !strings.Contains(exp.Content, "orbital pool") || strings.Contains(exp.Content, "excluded") {
		t.Fatalf("old HTML locator: %+v, error=%v", exp, err)
	}

	writeDoc(t, root, "one.md", "# One\n\nnew content for id-123\n")
	hits = search(t, a, "orbital")
	if len(hits.Candidates) != 1 || hits.Candidates[0].SourceRecordID != "two.html" || hits.Outcome != recall.SearchPartial {
		t.Fatalf("dirty corpus: %+v", hits)
	}
	h, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || h.Status != recall.HealthHealthy || h.IndexGeneration == first || fake.embedded != 3 {
		t.Fatalf("changed refresh: health=%+v, embedded=%d, err=%v", h, fake.embedded, err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, first)); !os.IsNotExist(err) {
		t.Fatalf("retired generation still exists: %v", err)
	}

	writeDoc(t, root, "two.html", "<meta name=robots content=noindex>\n<p>formerly available</p>\n")
	h, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || h.Status != recall.HealthHealthy || h.IndexedCount != 1 || fake.embedded != 3 {
		t.Fatalf("noindex refresh: health=%+v, embedded=%d, err=%v", h, fake.embedded, err)
	}
	_, err = a.Expand(ctx, recall.ExpandRequest{Locator: recall.Locator{Local: "two.html#L1-L2"}})
	if !errors.Is(err, protocol.ErrLocatorExpired) {
		t.Fatalf("withdrawn HTML evidence: %v", err)
	}
}

func TestFailedReopenKeepsPreviousGeneration(t *testing.T) {
	ctx := context.Background()
	a, _, root := fixture(t)
	writeDoc(t, root, "one.md", "orbital\n")
	first, err := a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || first.Status != recall.HealthHealthy {
		t.Fatalf("initialize index: %+v, %v", first, err)
	}
	open := a.open
	a.open = func(file string) (searchIndex, error) {
		if filepath.Base(filepath.Dir(file)) != first.IndexGeneration {
			return nil, errors.New("reopen failed")
		}
		return open(file)
	}
	writeDoc(t, root, "two.md", "orbital pool\n")
	next, err := a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || next.Status != recall.HealthDegraded || next.IndexGeneration != first.IndexGeneration {
		t.Fatalf("failed refresh: %+v, %v", next, err)
	}
	hits := search(t, a, "orbital")
	if hits.Outcome != recall.SearchPartial || len(hits.Candidates) != 1 || hits.Candidates[0].SourceRecordID != "one.md" {
		t.Fatalf("search after failed refresh: %+v", hits)
	}
	a.open = open
	next, err = a.Refresh(ctx, protocol.RefreshParams{})
	if err != nil || next.Status != recall.HealthHealthy || next.IndexedCount != 2 {
		t.Fatalf("recovered refresh: %+v, %v", next, err)
	}
}

func TestRefreshWithNoSearchableDocuments(t *testing.T) {
	a, fake, _ := fixture(t)
	h, err := a.Refresh(context.Background(), protocol.RefreshParams{})
	if err != nil || h.Status != recall.HealthHealthy || fake.embedded != 0 {
		t.Fatalf("empty refresh: health=%+v, embedded=%d, err=%v", h, fake.embedded, err)
	}
	got := search(t, a, "orbital")
	if got.Outcome != recall.SearchSuccess || len(got.Candidates) != 0 {
		t.Fatalf("empty search: %+v", got)
	}
}

type fixtureEmbeddings struct{ embedded int }

func (f *fixtureEmbeddings) Embed(_ context.Context, texts []string, query bool) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0, 0}
	}
	if !query {
		f.embedded += len(texts)
	}
	return vectors, nil
}

func (f *fixtureEmbeddings) EmbedDocuments(ctx context.Context, texts, _ []string) ([][]float32, error) {
	return f.Embed(ctx, texts, false)
}

type fixtureIndex []antfly.Row

func (f fixtureIndex) Search(_ context.Context, vector []float32, limit int) ([]antfly.Hit, error) {
	var hits []antfly.Hit
	for _, row := range f {
		var score float64
		for i, value := range row.Vector {
			score += float64(value * vector[i])
		}
		hits = append(hits, antfly.Hit{ID: row.ID, Score: score})
	}
	slices.SortFunc(hits, func(a, b antfly.Hit) int {
		if a.Score != b.Score {
			return int(math.Copysign(1, b.Score-a.Score))
		}
		return strings.Compare(a.ID, b.ID)
	})
	return hits[:min(limit, len(hits))], nil
}

func (fixtureIndex) Close() error { return nil }

func fixture(t *testing.T) (*Adapter, *fixtureEmbeddings, string) {
	t.Helper()
	root := t.TempDir()
	a := New()
	_, err := a.Initialize(context.Background(), adapter.Config{
		Location: root, Workdir: t.TempDir(), SourceID: "semantic",
		ProtocolVersionMin: protocol.MinVersion, ProtocolVersionMax: protocol.MaxVersion,
		Settings: map[string]any{"inference_url": "http://127.0.0.1:4000/v1", "inference_model": "fixture", "inference_dimensions": 3, "extensions": []string{".md", ".html"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fixtureEmbeddings{}
	a.embed = fake
	a.build = func(_ context.Context, file string, _ int, rows []antfly.Row) error {
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		return os.WriteFile(file, data, 0o600)
	}
	a.open = func(file string) (searchIndex, error) {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var rows fixtureIndex
		err = json.Unmarshal(data, &rows)
		return rows, err
	}
	return a, fake, root
}

func writeDoc(t *testing.T, root, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func search(t *testing.T, a *Adapter, query string) recall.SearchResponse {
	t.Helper()
	got, err := a.Search(context.Background(), recall.SearchRequest{Query: query, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	return got
}
