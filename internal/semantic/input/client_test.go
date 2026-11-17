package input

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEmbedWire(t *testing.T) {
	for _, query := range []bool{false, true} {
		t.Run(fmt.Sprint(query), func(t *testing.T) {
			t.Setenv("RECALL_TEST_EMBED_KEY", "fixture-secret")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("wrong request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				prefix, priority := "title: none | text: ", "background"
				if query {
					prefix, priority = "task: search result | query: ", "interactive"
				}
				want := map[string]any{
					"model": "gemma-fixture", "input": []any{prefix + "first", prefix + "second"}, "encoding_format": "float",
					"metadata": map[string]any{"rook": map[string]any{"localOnly": true, "priority": priority, "maxWaitSeconds": float64(0)}},
				}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("request body %#v, want %#v", body, want)
				}
				// Wire ordering must not change caller input ordering.
				fmt.Fprint(w, `{"model":"gemma-fixture","data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[0.6,0.8]}]}`)
			}))
			defer server.Close()
			for _, suffix := range []string{"", "/v1/"} {
				client, err := New(server.URL+suffix, "gemma-fixture", "RECALL_TEST_EMBED_KEY", 2)
				if err != nil {
					t.Fatal(err)
				}
				vectors, err := client.Embed(context.Background(), []string{"first", "second"}, query)
				if err != nil || !reflect.DeepEqual(vectors, [][]float32{{0.6, 0.8}, {0, 1}}) {
					t.Fatalf("vectors %v, %v", vectors, err)
				}
			}
		})
	}
}

func TestNewRejectsRemoteOrMalformedConfiguration(t *testing.T) {
	for _, u := range []string{"https://example.com", "http://192.168.1.1", "http://0.0.0.0", "http://localhost.example.com", "http://127.0.0.1.example.com", "http://[::]", "ftp://localhost", "http://user:secret@localhost", "http://localhost?key=secret", "http://localhost#secret", "http://localhost?", "://localhost", "/local"} {
		if _, err := New(u, "gemma", "", 2); err == nil {
			t.Errorf("accepted %q", u)
		}
	}
	for _, u := range []string{"http://localhost:1234", "http://127.0.0.1:1234", "http://127.0.0.2", "http://[::1]:1234", "http://[::ffff:127.0.0.1]:1234"} {
		if _, err := New(u, "gemma", "", 2); err != nil {
			t.Errorf("refused local %q: %v", u, err)
		}
	}
	for _, dims := range []int{0, -1, 65537} {
		if _, err := New("http://localhost", "gemma", "", dims); err == nil {
			t.Errorf("accepted dimensions %d", dims)
		}
	}
	if _, err := New("http://localhost", "", "", 2); err == nil {
		t.Fatal("empty model accepted")
	}
}

func TestEmbedDocumentsPreservesNormalizedTitles(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Input    []string `json:"input"`
			Metadata struct {
				Rook struct {
					Priority  string `json:"priority"`
					LocalOnly bool   `json:"localOnly"`
				} `json:"rook"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		want := []string{"title: The café notes | text: first\nparagraph", "title: none | text: second"}
		if !reflect.DeepEqual(body.Input, want) || body.Metadata.Rook.Priority != "background" || !body.Metadata.Rook.LocalOnly {
			t.Errorf("wrong titled document wire: %#v", body)
		}
		fmt.Fprint(w, `{"model":"gemma","data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[0,1]}]}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, "gemma", "", 2)
	if _, err := client.EmbedDocuments(context.Background(), []string{"first\nparagraph", "second"}, []string{" \tThe\ncafé\r\n notes\u2028", " \t\r\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedDocuments(context.Background(), []string{"first"}, nil); err == nil {
		t.Fatal("missing title entry accepted")
	}
	if _, err := client.EmbedDocuments(context.Background(), []string{"first"}, []string{"bad\xfftitle"}); err == nil {
		t.Fatal("invalid UTF-8 title accepted")
	}
	if _, err := client.EmbedDocuments(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected inference calls: %d", calls.Load())
	}
}

func TestEmbedRejectsMalformedResponses(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid JSON": "not JSON", "trailing JSON": `{} {}`,
		"wrong model":      `{"model":"other","data":[{"index":0,"embedding":[1,0]}]}`,
		"missing rows":     `{"model":"gemma","data":[]}`,
		"missing index":    `{"model":"gemma","data":[{"embedding":[1,0]}]}`,
		"null index":       `{"model":"gemma","data":[{"index":null,"embedding":[1,0]}]}`,
		"fractional index": `{"model":"gemma","data":[{"index":0.5,"embedding":[1,0]}]}`,
		"negative index":   `{"model":"gemma","data":[{"index":-1,"embedding":[1,0]}]}`,
		"outside index":    `{"model":"gemma","data":[{"index":1,"embedding":[1,0]}]}`,
		"wrong dims":       `{"model":"gemma","data":[{"index":0,"embedding":[1]}]}`,
		"null vector":      `{"model":"gemma","data":[{"index":0,"embedding":null}]}`,
		"null value":       `{"model":"gemma","data":[{"index":0,"embedding":[1,null]}]}`,
		"string value":     `{"model":"gemma","data":[{"index":0,"embedding":[1,"0"]}]}`,
		"overflow":         `{"model":"gemma","data":[{"index":0,"embedding":[1e300,0]}]}`,
		"infinity":         `{"model":"gemma","data":[{"index":0,"embedding":[1e999,0]}]}`,
		"zero vector":      `{"model":"gemma","data":[{"index":0,"embedding":[0,0]}]}`,
		"unnormalized":     `{"model":"gemma","data":[{"index":0,"embedding":[1,1]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, raw) }))
			defer server.Close()
			client, _ := New(server.URL, "gemma", "", 2)
			if vectors, err := client.Embed(context.Background(), []string{"private text"}, false); err == nil || vectors != nil {
				t.Fatalf("accepted malformed response %v: %v", vectors, err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"model":"gemma","data":[{"index":0,"embedding":[1,0]},{"index":0,"embedding":[0,1]}]}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, "gemma", "", 2)
	if _, err := client.Embed(context.Background(), []string{"one", "two"}, false); err == nil {
		t.Fatal("duplicate vector indices accepted")
	}
}

func TestEmbedDoesNotFollowRedirectsOrExposeBodies(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer target.Close()
	for _, status := range []int{302, 307, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(status)
			fmt.Fprint(w, "private response secret-key private-input")
		}))
		client, _ := New(server.URL, "gemma", "", 2)
		_, err := client.Embed(context.Background(), []string{"private-input"}, true)
		server.Close()
		if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "secret-key") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("followed provider redirect")
	}
}

func TestEmbedEmptyCredentialAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer server.Close()
	t.Setenv("RECALL_TEST_UNSET_EMBED_KEY", "")
	client, _ := New(server.URL, "gemma", "RECALL_TEST_UNSET_EMBED_KEY", 2)
	if out, err := client.Embed(context.Background(), nil, false); err != nil || len(out) != 0 {
		t.Fatalf("empty batch: %v, %v", out, err)
	}
	if _, err := client.Embed(context.Background(), []string{"text"}, false); err == nil {
		t.Fatal("missing credential accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Embed(ctx, []string{"text"}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("made request for empty/canceled batch or missing key")
	}
}
