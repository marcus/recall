package antfly

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

type fakeDB struct {
	operations []string
	inputs     map[string][]byte
	fail       string
	failure    error
	closed     int
	closeErr   error
	response   []byte
	metadata   []byte
	after      func(string)
}

func (db *fakeDB) call(op string, input []byte) ([]byte, error) {
	db.operations = append(db.operations, op)
	if db.inputs == nil {
		db.inputs = make(map[string][]byte)
	}
	db.inputs[op] = append([]byte(nil), input...)
	if db.after != nil {
		db.after(op)
	}
	if op == db.fail {
		return nil, db.failure
	}
	if op == "indexes" {
		return db.metadata, nil
	}
	return db.response, nil
}
func (db *fakeDB) close() error                            { db.closed++; return db.closeErr }
func (db *fakeDB) open(_ string, _ bool) (database, error) { return db, nil }
func sampleRows() []Row {
	return []Row{{ID: "doc:α", Vector: []float32{2, 1}}, {ID: "doc:β", Vector: []float32{1, 0}}}
}

func TestBuildContract(t *testing.T) {
	db := &fakeDB{}
	if err := build(context.Background(), "unused", 2, sampleRows(), db.open); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(db.operations, []string{"add_index", "batch", "drain"}) || db.closed != 1 {
		t.Fatalf("operations=%v closed=%d", db.operations, db.closed)
	}
	var index struct{ Name, Kind, ConfigJSON string }
	// Native envelope has snake_case config_json.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(db.inputs["add_index"], &envelope); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(envelope["name"], &index.Name)
	json.Unmarshal(envelope["kind"], &index.Kind)
	json.Unmarshal(envelope["config_json"], &index.ConfigJSON)
	var config struct {
		Field, Metric string
		Dims          int
		External      bool
	}
	if err := json.Unmarshal([]byte(index.ConfigJSON), &config); err != nil {
		t.Fatal(err)
	}
	if index.Name != indexName || index.Kind != "dense_vector" || config.Field != "embedding" || config.Dims != 2 || config.Metric != "cosine" || !config.External {
		t.Fatalf("unexpected index: %+v config: %+v", index, config)
	}
	var batch struct {
		Inserts map[string]struct {
			ID         string               `json:"id"`
			Embeddings map[string][]float32 `json:"_embeddings"`
		} `json:"inserts"`
		SyncLevel string `json:"sync_level"`
	}
	if err := json.Unmarshal(db.inputs["batch"], &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Inserts) != 2 || batch.SyncLevel != "write" {
		t.Fatalf("batch: %+v", batch)
	}
	for _, row := range sampleRows() {
		stored := batch.Inserts[row.ID]
		if stored.ID != row.ID || !reflect.DeepEqual(stored.Embeddings[indexName], row.Vector) {
			t.Fatalf("lost row: %+v", stored)
		}
	}
}

func TestBuildEmptyAndFailures(t *testing.T) {
	failure := errors.New("native failure")
	for _, op := range []string{"", "add_index", "batch", "drain"} {
		t.Run(op, func(t *testing.T) {
			db := &fakeDB{fail: op, failure: failure}
			err := build(context.Background(), "unused", 2, sampleRows(), db.open)
			if (op != "" && !errors.Is(err, failure)) || (op == "" && err != nil) {
				t.Fatalf("build error: %v", err)
			}
			if db.closed != 1 {
				t.Fatalf("close count %d", db.closed)
			}
			if op != "" && db.operations[len(db.operations)-1] != op {
				t.Fatalf("continued after failure: %v", db.operations)
			}
		})
	}
	db := &fakeDB{closeErr: failure}
	if err := build(context.Background(), "unused", 2, nil, db.open); !errors.Is(err, failure) {
		t.Fatalf("lost close error: %v", err)
	}
	if !reflect.DeepEqual(db.operations, []string{"add_index", "drain"}) {
		t.Fatalf("empty operations %v", db.operations)
	}
	_, openErr := openIndex("unused", func(string, bool) (database, error) { return nil, failure })
	if !errors.Is(openErr, failure) {
		t.Fatalf("lost open error: %v", openErr)
	}
}

func TestInputValidationBeforeOpening(t *testing.T) {
	tests := []struct {
		name, path string
		dims       int
		rows       []Row
	}{
		{"path", "", 2, sampleRows()}, {"nul path", "bad\x00path", 2, sampleRows()}, {"dimensions", "unused", 0, nil},
		{"duplicate", "unused", 2, []Row{{"same", []float32{1, 0}}, {"same", []float32{0, 1}}}},
		{"id", "unused", 2, []Row{{"", []float32{1, 0}}}},
		{"invalid UTF-8 ID", "unused", 2, []Row{{string([]byte{0xff}), []float32{1, 0}}}},
		{"size", "unused", 3, sampleRows()}, {"zero", "unused", 2, []Row{{"zero", []float32{0, 0}}}},
		{"nan", "unused", 2, []Row{{"nan", []float32{float32(math.NaN()), 0}}}},
		{"infinity", "unused", 2, []Row{{"inf", []float32{float32(math.Inf(1)), 0}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := build(context.Background(), test.path, test.dims, test.rows, func(string, bool) (database, error) { t.Fatal("invalid input opened database"); return nil, nil })
			if err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func TestCancellationAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := &fakeDB{}
	if err := build(ctx, "unused", 2, sampleRows(), db.open); !errors.Is(err, context.Canceled) || len(db.operations) != 0 || db.closed != 0 {
		t.Fatalf("pre-cancel: %v db=%+v", err, db)
	}
	ctx, cancel = context.WithCancel(context.Background())
	db = &fakeDB{after: func(op string) {
		if op == "batch" {
			cancel()
		}
	}}
	if err := build(ctx, "unused", 2, sampleRows(), db.open); !errors.Is(err, context.Canceled) || db.closed != 1 || !reflect.DeepEqual(db.operations, []string{"add_index", "batch"}) {
		t.Fatalf("mid-cancel: %v db=%+v", err, db)
	}
}

func TestOpenMetadata(t *testing.T) {
	metadata := `[{"name":"dv_embedding_v1","kind":"dense_vector","config_json":"{\"dims\":2,\"metric\":\"cosine\",\"external\":true}"}]`
	db := &fakeDB{metadata: []byte(metadata)}
	idx, err := openIndex("unused", db.open)
	if err != nil {
		t.Fatal(err)
	}
	if idx.dimensions != 2 || db.closed != 0 {
		t.Fatalf("index=%+v closed=%d", idx, db.closed)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{`, `[]`, `[{"name":"dv_embedding_v1","kind":"dense_vector","config_json":"{}"}]`} {
		db := &fakeDB{metadata: []byte(bad)}
		if _, err := openIndex("unused", db.open); err == nil || db.closed != 1 {
			t.Fatalf("bad metadata accepted or handle leaked: %q error=%v closed=%d", bad, err, db.closed)
		}
	}
}

func TestSearchContractAndClose(t *testing.T) {
	db := &fakeDB{response: []byte(`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a","_score":1},{"_id":"b","_score":0.799},{"_id":"c","_score":0}]}}]}`)}
	idx := &Index{db: db, dimensions: 2}
	hits, err := idx.Search(context.Background(), []float32{2, 1}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hits, []Hit{{"a", 1}, {"b", .799}, {"c", 0}}) {
		t.Fatalf("hits=%v", hits)
	}
	var request struct {
		Embeddings map[string][]float32
		Indexes    []string
		Limit      int
		Effort     float64 `json:"search_effort"`
	}
	if err := json.Unmarshal(db.inputs["search"], &request); err != nil {
		t.Fatal(err)
	}
	if request.Effort != 1 || request.Limit != 3 || !reflect.DeepEqual(request.Indexes, []string{indexName}) || !reflect.DeepEqual(request.Embeddings[indexName], []float32{2, 1}) {
		t.Fatalf("request %+v", request)
	}
	if hits, err := idx.Search(context.Background(), []float32{1, 0}, 0); err != nil || hits == nil || len(db.operations) != 1 {
		t.Fatalf("zero limit hits=%v error=%v operations=%v", hits, err, db.operations)
	}
	for _, test := range []struct {
		vector []float32
		limit  int
	}{{[]float32{1, 0}, -1}, {[]float32{1}, 1}, {[]float32{0, 0}, 1}} {
		if _, err := idx.Search(context.Background(), test.vector, test.limit); err == nil {
			t.Fatal("invalid search accepted")
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); idx.Close() }()
	}
	wg.Wait()
	if db.closed != 1 {
		t.Fatalf("closed %d times", db.closed)
	}
	if _, err := idx.Search(context.Background(), []float32{1, 0}, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed search error %v", err)
	}
	var nilIndex *Index
	if err := nilIndex.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := nilIndex.Search(context.Background(), []float32{1}, 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil search %v", err)
	}
}

func TestDecodeRejectsNativeFailures(t *testing.T) {
	for _, data := range []string{
		`{`, `{}`, `{"responses":[]}`, `{"responses":[{"status":500,"error":"failed"}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a"}]}}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a","_score":1.01}]}}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a","_score":-1.01}]}}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"","_score":1}]}}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a","_score":1},{"_id":"a","_score":1}]}}]}`,
		`{"responses":[{"status":200,"hits":{"hits":[{"_id":"a","_score":1},{"_id":"b","_score":1}]}}]}`,
	} {
		if _, err := decodeHits([]byte(data), 1); err == nil {
			t.Fatalf("bad native response accepted: %s", data)
		}
	}
}
