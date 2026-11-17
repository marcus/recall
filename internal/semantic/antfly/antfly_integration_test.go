//go:build antflyintegration && cgo && darwin && arm64

package antfly

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"testing"
)

func TestNativeBuildReopenSearch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.aflite")
	rows := []Row{{"same/α", []float32{2, 0, 0}}, {"near", []float32{4, 3, 0}}, {"orthogonal", []float32{0, 2, 0}}, {"opposite", []float32{-2, 0, 0}}}
	if err := Build(ctx, path, 3, rows); err != nil {
		t.Fatal(err)
	}
	if err := Build(ctx, path, 3, rows); err == nil {
		t.Fatal("rebuild overwrote existing index")
	}
	for range 2 {
		idx, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		hits, err := idx.Search(ctx, []float32{7, 0, 0}, 4)
		if err != nil {
			idx.Close()
			t.Fatal(err)
		}
		t.Logf("native hits: %+v", hits)
		if len(hits) != 4 || hits[0].ID != "same/α" || hits[0].Score != 1 || hits[1].ID != "near" || math.Abs(hits[1].Score-.8) > .002 {
			idx.Close()
			t.Fatalf("unexpected cosine ranking: %+v", hits)
		}
		if hits[2].ID != "orthogonal" || hits[2].Score != 0 || hits[3].ID != "opposite" || hits[3].Score != -1 {
			t.Errorf("signed cosine lost: %+v", hits)
		}
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
		if err := idx.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.Search(ctx, []float32{1, 0, 0}, 1); !errors.Is(err, ErrClosed) {
			t.Fatalf("search after close: %v", err)
		}
	}
}

func TestNativeEmptyIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.aflite")
	if err := Build(context.Background(), path, 3, nil); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	hits, err := idx.Search(context.Background(), []float32{1, 0, 0}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("empty hits=%v err=%v", hits, err)
	}
}

// Guard the fresh hosted single-batch path against silent incomplete coverage,
// and prove effort=1 against a brute-force oracle after the writer has closed.
func TestNativeExactAgainstCosine(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(71, 19))
	rows := make([]Row, 256)
	for n := range rows {
		vector := make([]float32, 8)
		for i := range vector {
			vector[i] = float32(rng.Float64()*2 - 1)
		}
		rows[n] = Row{ID: fmt.Sprintf("row-%03d", n), Vector: vector}
	}
	path := filepath.Join(t.TempDir(), "oracle.aflite")
	if err := Build(ctx, path, 8, rows); err != nil {
		t.Fatal(err)
	}
	idx, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	for _, q := range []int{0, 37, 81, 143, 255} {
		query := rows[q].Vector
		want := make([]Hit, len(rows))
		for i, row := range rows {
			var dot, qa, rb float64
			for d, v := range query {
				dot += float64(v) * float64(row.Vector[d])
				qa += float64(v) * float64(v)
				rb += float64(row.Vector[d]) * float64(row.Vector[d])
			}
			want[i] = Hit{ID: row.ID, Score: dot / math.Sqrt(qa*rb)}
		}
		sort.Slice(want, func(i, j int) bool { return want[i].Score > want[j].Score })
		got, err := idx.Search(ctx, query, 12)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 12 {
			t.Fatalf("query %d: %d hits, want 12", q, len(got))
		}
		for i, hit := range got {
			if hit.ID != want[i].ID || math.Abs(hit.Score-want[i].Score) > 1e-5 {
				t.Fatalf("query %d rank %d got %+v want %+v", q, i, hit, want[i])
			}
		}
	}
}
