// Package input provides local-only embedding inference for semantic adapters.
// Corpus extraction and vector storage remain independent of this transport.
package input

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Client speaks Rook's OpenAI embeddings wire contract, without retries,
// provider discovery or paid fallback. It is safe for concurrent use.
type Client struct {
	endpoint   string
	model      string
	apiKeyEnv  string
	dimensions int
	http       *http.Client
}

// New validates configuration without reading credentials or making requests.
// Only literal loopback IPs and localhost are accepted; proxies and redirects
// are disabled and localhost is dialed directly on loopback to avoid DNS drift.
func New(rawURL, model, apiKeyEnv string, dimensions int) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("embedding: invalid local provider URL")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("embedding: provider must use a loopback address")
	}
	if model == "" || strings.TrimSpace(model) != model || !utf8.ValidString(model) || dimensions < 1 || dimensions > 65536 {
		return nil, errors.New("embedding: model and dimensions are invalid")
	}
	if strings.ContainsAny(apiKeyEnv, "=\x00") {
		return nil, errors.New("embedding: invalid credential environment variable name")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/embeddings"
	} else {
		u.Path += "/v1/embeddings"
	}
	u.RawPath = ""
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid local embedding address")
		}
		if strings.EqualFold(host, "localhost") {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
			if err == nil {
				return conn, nil
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("::1", port))
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, errors.New("embedding address is not loopback")
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &Client{
		endpoint: u.String(), model: model, apiKeyEnv: apiKeyEnv, dimensions: dimensions,
		http: &http.Client{
			Transport: transport, Timeout: 45 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("embedding redirects are forbidden") },
		},
	}, nil
}

// Embed formats raw text with the exact EmbeddingGemma search prefixes. The
// document interface has no separate title, so its title field is "none".
// Queries are interactive; refreshes run at background priority and never wait
// for unavailable capacity. Both require localOnly, with no fallback request.
func (c *Client) Embed(ctx context.Context, texts []string, query bool) ([][]float32, error) {
	prefix, priority := "title: none | text: ", "background"
	if query {
		prefix, priority = "task: search result | query: ", "interactive"
	}
	formatted := make([]string, len(texts))
	for i, text := range texts {
		formatted[i] = prefix + text
	}
	return c.embed(ctx, formatted, priority)
}

// EmbedDocuments preserves QMD's per-document title signal during background
// refresh. Title whitespace is collapsed to a single line; blank titles use
// EmbeddingGemma's "none" fallback. Texts and titles must have equal lengths.
func (c *Client) EmbedDocuments(ctx context.Context, texts, titles []string) ([][]float32, error) {
	if len(texts) != len(titles) {
		return nil, errors.New("embedding: document title count mismatch")
	}
	formatted := make([]string, len(texts))
	for i, text := range texts {
		title := strings.Join(strings.Fields(titles[i]), " ")
		if title == "" {
			title = "none"
		}
		formatted[i] = "title: " + title + " | text: " + text
	}
	return c.embed(ctx, formatted, "background")
}

func (c *Client) embed(ctx context.Context, formatted []string, priority string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(formatted) == 0 {
		return [][]float32{}, nil
	}
	for _, text := range formatted {
		if !utf8.ValidString(text) {
			return nil, errors.New("embedding: input is not valid UTF-8")
		}
	}
	body, err := json.Marshal(struct {
		Model    string         `json:"model"`
		Input    []string       `json:"input"`
		Encoding string         `json:"encoding_format"`
		Metadata map[string]any `json:"metadata"`
	}{c.model, formatted, "float", map[string]any{"rook": map[string]any{"localOnly": true, "priority": priority, "maxWaitSeconds": 0}}})
	if err != nil {
		return nil, errors.New("embedding: cannot encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embedding: cannot construct request")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKeyEnv != "" {
		key := os.Getenv(c.apiKeyEnv)
		if key == "" {
			return nil, errors.New("embedding: credential environment variable is unset")
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Do not expose request text, keys, provider URLs or response bodies.
		return nil, errors.New("embedding: local provider is unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding: local provider returned status %d", response.StatusCode)
	}
	const maxResponseBytes = 64 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("embedding: cannot read provider response")
	}
	if len(raw) > maxResponseBytes {
		return nil, errors.New("embedding: provider response exceeds byte limit")
	}
	var result struct {
		Model string `json:"model"`
		Data  []struct {
			Index     *int       `json:"index"`
			Embedding []*float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, errors.New("embedding: provider returned invalid JSON")
	}
	if result.Model != c.model {
		return nil, errors.New("embedding: model identity mismatch")
	}
	if len(result.Data) != len(formatted) {
		return nil, errors.New("embedding: vector count mismatch")
	}
	out := make([][]float32, len(formatted))
	for _, row := range result.Data {
		if row.Index == nil || *row.Index < 0 || *row.Index >= len(out) || out[*row.Index] != nil {
			return nil, errors.New("embedding: vector indices are invalid")
		}
		if len(row.Embedding) != c.dimensions {
			return nil, errors.New("embedding: vector dimensions are invalid")
		}
		vector, norm := make([]float32, c.dimensions), 0.0
		for j, value := range row.Embedding {
			if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || math.Abs(*value) > math.MaxFloat32 {
				return nil, errors.New("embedding: vector values are invalid")
			}
			vector[j] = float32(*value)
			norm = math.Hypot(norm, *value)
		}
		if math.Abs(norm-1) > 0.01 {
			return nil, errors.New("embedding: vector is not L2 normalized")
		}
		out[*row.Index] = vector
	}
	return out, nil
}
