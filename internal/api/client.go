package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/marcus/recall/pkg/recall"
)

// Client calls a running `recall serve` and satisfies [Core].
//
// It exists so that dispatching to a server is a substitution rather than a
// second code path. The local API's whole justification is amortizing adapter
// cold start, and that is only worth anything if the answers are the same ones
// the in-process core would have given — so the client returns the same domain
// types, reproduces the same outcome and coverage, and turns the same failures
// into the same closed vocabulary. Anything it had to reinterpret would be a
// place the two paths could disagree.
type Client struct {
	base    string
	profile string
	token   string
	http    *http.Client
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// BaseURL is the server root, such as http://127.0.0.1:8765.
	BaseURL string

	// Profile is the profile the caller believes it is asking. It is sent with
	// every request so a server that serves a different one refuses rather than
	// answering the wrong question. Empty means "whatever this server serves".
	Profile string

	// HTTP is the client to use. Nil means [http.DefaultClient].
	HTTP *http.Client

	// BearerToken authenticates to a server configured for non-loopback
	// access. It is sent in an Authorization header and never in the URL.
	BearerToken string
}

// NewClient builds a client for a running server.
func NewClient(opt ClientOptions) *Client {
	c := &Client{
		base:    strings.TrimSuffix(opt.BaseURL, "/"),
		profile: opt.Profile,
		token:   opt.BearerToken,
		http:    opt.HTTP,
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	return c
}

var _ Core = (*Client)(nil)

// Profile reports the profile this client asks for. See [ClientOptions].
func (c *Client) Profile() string { return c.profile }

// Query sends a search and returns the response the server produced.
//
// A failed query — every source that was asked failed — arrives as HTTP 503
// carrying a complete response, and is returned as a response rather than as an
// error. That is the distinction this whole surface exists to preserve: the
// request succeeded, the corpus could not answer it, and the caller needs the
// source outcomes to see why. A genuine transport or request failure has no
// outcome header and is returned as an error.
func (c *Client) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	if req.Profile == "" {
		req.Profile = c.profile
	}
	resp, body, err := c.do(ctx, http.MethodPost, "/query", req)
	if err != nil {
		return recall.QueryResponse{}, err
	}
	if resp.Header.Get(HeaderOutcome) == "" {
		return recall.QueryResponse{}, problemFrom(resp, body)
	}
	var out recall.QueryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return recall.QueryResponse{}, fmt.Errorf("decoding query response: %w", err)
	}
	return out, nil
}

// Expand retrieves the evidence behind a locator.
func (c *Client) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	body := struct {
		recall.ExpandRequest
		Profile string `json:"profile,omitempty"`
	}{req, c.profile}

	resp, raw, err := c.do(ctx, http.MethodPost, "/expand", body)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return recall.ExpandResponse{}, problemFrom(resp, raw)
	}
	var out recall.ExpandResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return recall.ExpandResponse{}, fmt.Errorf("decoding expand response: %w", err)
	}
	return out, nil
}

// Refresh updates adapter-owned indexes through a running server.
func (c *Client) Refresh(ctx context.Context, req recall.RefreshRequest) (recall.RefreshResponse, error) {
	if req.Profile == "" {
		req.Profile = c.profile
	}
	resp, body, err := c.do(ctx, http.MethodPost, "/refresh", req)
	if err != nil {
		return recall.RefreshResponse{}, err
	}
	if resp.Header.Get(HeaderOutcome) == "" {
		return recall.RefreshResponse{}, problemFrom(resp, body)
	}
	var out recall.RefreshResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return recall.RefreshResponse{}, fmt.Errorf("decoding refresh response: %w", err)
	}
	return out, nil
}

// Sources lists the configured sources as the server reports them.
func (c *Client) Sources(ctx context.Context) (Listing, error) { return c.listing(ctx, "/sources") }

// Doctor runs the server's diagnosis.
func (c *Client) Doctor(ctx context.Context) (Listing, error) { return c.listing(ctx, "/doctor") }

// Identity reports what the server is: build, API version, and served profile.
func (c *Client) Identity(ctx context.Context) (Identity, error) {
	resp, body, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return Identity{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, problemFrom(resp, body)
	}
	var out Identity
	if err := json.Unmarshal(body, &out); err != nil {
		return Identity{}, fmt.Errorf("decoding version response: %w", err)
	}
	return out, nil
}

// listing carries a listing back without parsing it.
//
// The payload is kept as raw JSON on purpose. The client does not know the
// shape of a source listing or a diagnosis and must not learn it: re-marshalling
// through a local struct is exactly how a field added on the server would go
// missing on the way through a client that was built before it existed. Held
// raw, the bytes a caller receives are the bytes the server produced.
func (c *Client) listing(ctx context.Context, path string) (Listing, error) {
	resp, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Listing{}, err
	}
	status := Status(resp.Header.Get(HeaderCoverage))
	switch status {
	case StatusOK, StatusDegraded, StatusFailed:
	default:
		return Listing{}, problemFrom(resp, body)
	}
	return Listing{Payload: json.RawMessage(body), Status: status}, nil
}

type wireResponse struct {
	StatusCode int
	Status     string
	Header     http.Header
}

func (c *Client) do(ctx context.Context, method, path string, body any) (wireResponse, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return wireResponse{}, nil, fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+"/"+Version+path, reader)
	if err != nil {
		return wireResponse{}, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return wireResponse{}, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return wireResponse{}, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if served := resp.Header.Get(HeaderProfile); c.profile != "" && served != "" && served != c.profile {
		return wireResponse{}, nil, Problem{CodeProfileMismatch, fmt.Sprintf(
			"server serves profile %q, not %q", served, c.profile)}
	}
	return wireResponse{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}, raw, nil
}

// MaxResponseBytes bounds what a client will read from a server.
//
// Responses are budget-shaped by the core, so this is a guard against a
// misbehaving or impersonated server rather than a limit a real answer meets.
const MaxResponseBytes = 64 << 20

// problemFrom recovers the server's closed-vocabulary error, falling back to
// the status line when the body is not one.
func problemFrom(resp wireResponse, body []byte) error {
	var wrapper struct {
		Error Problem `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Error.Code != "" {
		return wrapper.Error
	}
	return Problem{CodeFailed, fmt.Sprintf("server returned %s", resp.Status)}
}
