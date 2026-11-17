// Package antfly builds immutable, externally embedded Antfly Lite indexes.
package antfly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode/utf8"
)

const indexName = "dv_embedding_v1"

// ErrUnavailable means the optional, pinned native runtime cannot be loaded.
var ErrUnavailable = errors.New("antfly: native runtime unavailable")

// ErrClosed means Search was called after Close.
var ErrClosed = errors.New("antfly: index closed")

// Row supplies an opaque document ID and a finite, nonzero embedding.
type Row struct {
	ID     string
	Vector []float32
}

// Hit contains the document ID and signed cosine similarity in [-1,1].
// The pinned Lite runtime returns cosine directly, including negative values.
// Small floating-point excursions past the endpoints are clamped.
type Hit struct {
	ID    string
	Score float64
}

// Index is a read-only native index. Search and Close may be called concurrently.
// Callers must Close indexes to release native handles and reader leases.
type Index struct {
	mu         sync.Mutex
	db         database
	dimensions int
}

type database interface {
	call(operation string, input []byte) ([]byte, error)
	close() error
}
type opener func(path string, create bool) (database, error)

// Build creates a new .aflite file: hosted create, one synchronous batch,
// drain, close. The destination must not exist. Failure may leave an incomplete
// file; staging, cleanup and publication belong to the caller. Empty rows are
// supported. The parent directory must already exist.
func Build(ctx context.Context, path string, dimensions int, rows []Row) error {
	return build(ctx, path, dimensions, rows, openNative)
}

func build(ctx context.Context, path string, dimensions int, rows []Row, open opener) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = validPath(path); err != nil {
		return err
	}
	if dimensions <= 0 {
		return errors.New("antfly: dimensions must be positive")
	}
	inserts := make(map[string]any, len(rows))
	for _, row := range rows {
		if err = ctx.Err(); err != nil {
			return err
		}
		if row.ID == "" || !utf8.ValidString(row.ID) {
			return errors.New("antfly: row ID must be nonempty valid UTF-8")
		}
		if _, exists := inserts[row.ID]; exists {
			return fmt.Errorf("antfly: duplicate row ID %q", row.ID)
		}
		if err = validVector(row.Vector, dimensions); err != nil {
			return fmt.Errorf("antfly: row %q: %w", row.ID, err)
		}
		inserts[row.ID] = map[string]any{"id": row.ID, "_embeddings": map[string][]float32{indexName: row.Vector}}
	}
	config, _ := json.Marshal(map[string]any{"field": "embedding", "dims": dimensions, "metric": "cosine", "external": true})
	index, _ := json.Marshal(map[string]any{"name": indexName, "kind": "dense_vector", "config_json": string(config)})
	batch, err := json.Marshal(map[string]any{"inserts": inserts, "sync_level": "write"})
	if err != nil {
		return fmt.Errorf("antfly: encode batch: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	db, err := open(path, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.close()) }()
	if _, err = invoke(ctx, db, "add_index", index); err != nil {
		return err
	}
	if len(rows) > 0 {
		if _, err = invoke(ctx, db, "batch", batch); err != nil {
			return err
		}
	}
	_, err = invoke(ctx, db, "drain", nil)
	return err
}

// Open opens an existing engine-created index read-only.
func Open(path string) (*Index, error) { return openIndex(path, openNative) }

func openIndex(path string, open opener) (*Index, error) {
	if err := validPath(path); err != nil {
		return nil, err
	}
	db, err := open(path, false)
	if err != nil {
		return nil, err
	}
	data, err := db.call("indexes", nil)
	if err == nil {
		var indexes []struct {
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Config string `json:"config_json"`
		}
		err = json.Unmarshal(data, &indexes)
		if err == nil {
			for _, index := range indexes {
				if index.Name != indexName || index.Kind != "dense_vector" {
					continue
				}
				var config struct {
					Dims     int    `json:"dims"`
					Metric   string `json:"metric"`
					External bool   `json:"external"`
				}
				if err = json.Unmarshal([]byte(index.Config), &config); err != nil {
					break
				}
				if config.Dims > 0 && config.Metric == "cosine" && config.External {
					return &Index{db: db, dimensions: config.Dims}, nil
				}
			}
		}
	}
	if err == nil {
		err = errors.New("missing or incompatible cosine index")
	}
	return nil, errors.Join(fmt.Errorf("antfly: read index metadata: %w", err), db.close())
}

// Search uses exact search_effort=1, returning native rank order and at most
// limit hits. A zero limit returns an empty slice. Cancellation is checked
// before and after each native call; an in-progress C call cannot be interrupted.
func (idx *Index) Search(ctx context.Context, vector []float32, limit int) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if idx == nil {
		return nil, ErrClosed
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return nil, ErrClosed
	}
	if limit < 0 {
		return nil, errors.New("antfly: limit must not be negative")
	}
	if err := validVector(vector, idx.dimensions); err != nil {
		return nil, fmt.Errorf("antfly: query: %w", err)
	}
	if limit == 0 {
		return []Hit{}, nil
	}
	request, _ := json.Marshal(map[string]any{"embeddings": map[string][]float32{indexName: vector}, "indexes": []string{indexName}, "limit": limit, "search_effort": 1})
	data, err := invoke(ctx, idx.db, "search", request)
	if err != nil {
		return nil, err
	}
	return decodeHits(data, limit)
}

func decodeHits(data []byte, limit int) ([]Hit, error) {
	var response struct {
		Responses []struct {
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
			Hits   struct {
				Hits []struct {
					ID    string   `json:"_id"`
					Score *float64 `json:"_score"`
				} `json:"hits"`
			} `json:"hits"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("antfly: decode search: %w", err)
	}
	if len(response.Responses) != 1 {
		return nil, errors.New("antfly: expected one search response")
	}
	result := response.Responses[0]
	if result.Status != 200 {
		return nil, fmt.Errorf("antfly: search status %d: %s", result.Status, result.Error)
	}
	hits := make([]Hit, 0, len(result.Hits.Hits))
	seen := make(map[string]bool, len(result.Hits.Hits))
	for _, hit := range result.Hits.Hits {
		if hit.ID == "" || seen[hit.ID] || hit.Score == nil || math.IsNaN(*hit.Score) || math.IsInf(*hit.Score, 0) || *hit.Score < -1.00001 || *hit.Score > 1.00001 {
			return nil, errors.New("antfly: invalid native search hit")
		}
		seen[hit.ID] = true
		hits = append(hits, Hit{ID: hit.ID, Score: math.Max(-1, math.Min(1, *hit.Score))})
	}
	if len(hits) > limit {
		return nil, errors.New("antfly: search exceeded requested limit")
	}
	return hits, nil
}

// Close waits for Search, releases native resources, and is idempotent.
func (idx *Index) Close() error {
	if idx == nil {
		return nil
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.db == nil {
		return nil
	}
	err := idx.db.close()
	idx.db = nil
	return err
}

func invoke(ctx context.Context, db database, operation string, input []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := db.call(operation, input)
	if err != nil {
		return nil, fmt.Errorf("antfly: %s: %w", operation, err)
	}
	return data, ctx.Err()
}

func validPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) {
		return errors.New("antfly: invalid index path")
	}
	return nil
}
func validVector(vector []float32, dimensions int) error {
	if len(vector) != dimensions {
		return fmt.Errorf("vector dimensions %d, want %d", len(vector), dimensions)
	}
	var norm float64
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("vector contains nonfinite value")
		}
		norm += v * v
	}
	if norm == 0 {
		return errors.New("zero vector has no cosine similarity")
	}
	return nil
}
