package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Conn is an [Adapter] reached over the protocol on an already-connected
// stream.
//
// It holds no process knowledge, so the same request path serves a subprocess
// and an in-process pipe. [External] adds supervision on top by installing an
// escalation hook; without one, a wedged peer still fails as a timeout, there
// is simply nothing to signal.
type Conn struct {
	name   string
	client *protocol.Client
	diag   *protocol.Diagnostics

	healthTTL time.Duration
	termGrace time.Duration

	// sem bounds in-flight requests to the manifest's max_concurrency. A nil
	// channel means the adapter declared no limit.
	sem chan struct{}

	manifest  recall.Manifest
	coldStart time.Duration

	probeMu sync.Mutex

	mu           sync.Mutex
	cached       healthEntry
	cachedAt     time.Time
	coldReported bool

	// escalate runs when a request outlives its deadline without the adapter
	// answering the cancel notification. It is how a wedged process gets
	// signalled; a pipe transport leaves it nil.
	escalate func(reason string)

	closeOnce sync.Once
	closeErr  error
}

// Connect performs the handshake over an existing stream and returns the
// adapter behind it.
//
// w is closed by [Conn.Close] so the peer sees EOF on its stdin. A handshake
// that cannot agree on a protocol version fails here; there is no partially
// initialized Conn.
func Connect(ctx context.Context, r io.Reader, w io.WriteCloser, cfg Config, opt Options) (*Conn, error) {
	return connect(ctx, r, w, cfg, opt, 0)
}

func connect(ctx context.Context, r io.Reader, w io.WriteCloser, cfg Config, opt Options, coldStart time.Duration) (*Conn, error) {
	diag := opt.Diagnostics
	if diag == nil {
		diag = protocol.NewDiagnostics()
	}
	client, err := protocol.NewClient(r, w, protocol.ClientOptions{
		Diagnostics: diag,
		CancelGrace: opt.CancelGrace,
		MaxFrame:    opt.MaxFrame,
		Closer:      w,
	})
	if err != nil {
		return nil, err
	}

	c := &Conn{
		name:      "adapter",
		client:    client,
		diag:      diag,
		healthTTL: opt.healthTTL(),
		termGrace: opt.termGrace(),
		coldStart: coldStart,
	}

	if cfg.ProtocolVersionMin == 0 && cfg.ProtocolVersionMax == 0 {
		cfg.ProtocolVersionMin, cfg.ProtocolVersionMax = protocol.MinVersion, protocol.MaxVersion
	}

	handshake, cancel := context.WithTimeout(ctx, opt.handshakeTimeout())
	defer cancel()

	var manifest recall.Manifest
	if err := client.Call(handshake, protocol.MethodInitialize, cfg, &manifest); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if err := protocol.CheckNegotiated(cfg, manifest.ProtocolVersion); err != nil {
		// An adapter naming a version outside the requested range has not
		// agreed to anything. Continuing would mean speaking a dialect neither
		// end implements, so the handshake fails instead of degrading.
		_ = client.Close()
		return nil, err
	}

	c.manifest = manifest
	if manifest.AdapterID != "" {
		c.name = manifest.AdapterID
	}
	if manifest.MaxConcurrency > 0 {
		c.sem = make(chan struct{}, manifest.MaxConcurrency)
	}
	return c, nil
}

// Manifest returns what the adapter declared at handshake.
func (c *Conn) Manifest() recall.Manifest { return c.manifest }

// ColdStart returns how long this connection took to become ready. It counts
// against the request budget that paid for it and is reported separately from
// warm latency.
func (c *Conn) ColdStart() time.Duration { return c.coldStart }

// Diagnostics returns the peer's captured stderr and protocol violations.
func (c *Conn) Diagnostics() *protocol.Diagnostics { return c.diag }

// Initialize returns the negotiated manifest.
//
// Negotiation happens once, at connect time: a live connection has already
// agreed on a version and cannot renegotiate. cfg is therefore ignored here,
// and is present because the same interface is implemented by built-in
// adapters that have nothing connected yet.
func (c *Conn) Initialize(context.Context, Config) (recall.Manifest, error) {
	return c.manifest, nil
}

// acquire bounds in-flight requests to what the manifest declared. An adapter
// that says max_concurrency: 1 gets exactly one request at a time.
func (c *Conn) acquire(ctx context.Context) (func(), error) {
	if c.sem == nil {
		return func() {}, nil
	}
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deadlineCtx applies the request's own deadline. An elapsed deadline is left
// to expire immediately rather than being silently extended.
func deadlineCtx(ctx context.Context, at time.Time) (context.Context, context.CancelFunc) {
	if at.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, at)
}

// Search asks the adapter for candidates.
//
// Every failure returns both an error and a response whose outcome says what
// happened. A caller that inspects only one of the two still cannot mistake an
// unreachable source for a source with no matches.
func (c *Conn) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	ctx, cancel := deadlineCtx(ctx, req.Deadline)
	defer cancel()

	release, err := c.acquire(ctx)
	if err != nil {
		return FailedSearch(err), fmt.Errorf("%s: waiting for a slot: %w", c.name, err)
	}
	defer release()

	var out recall.SearchResponse
	if err := c.client.Call(ctx, protocol.MethodSearch, req, &out); err != nil {
		c.noteFailure(err)
		return FailedSearch(err), fmt.Errorf("%s: search: %w", c.name, err)
	}
	if !knownOutcome(out.Outcome) {
		err := protocol.Errorf(protocol.CodeInternal, "unknown outcome %q", out.Outcome)
		return FailedSearch(err), fmt.Errorf("%s: search: %w", c.name, err)
	}
	return out, nil
}

// Expand retrieves evidence behind a locator.
func (c *Conn) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	ctx, cancel := deadlineCtx(ctx, req.Deadline)
	defer cancel()

	release, err := c.acquire(ctx)
	if err != nil {
		return recall.ExpandResponse{}, fmt.Errorf("%s: waiting for a slot: %w", c.name, err)
	}
	defer release()

	var out recall.ExpandResponse
	if err := c.client.Call(ctx, protocol.MethodExpand, req, &out); err != nil {
		c.noteFailure(err)
		return recall.ExpandResponse{}, fmt.Errorf("%s: expand: %w", c.name, err)
	}
	return out, nil
}

// Health probes the source, reusing a recent probe when one is within the TTL.
//
// Probes are serialized: a burst of concurrent queries against one source
// should cost one probe, not one per query.
func (c *Conn) Health(ctx context.Context) (recall.Health, error) {
	if entry, ok := c.fresh(); ok {
		return entry.health, entry.err
	}

	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	// Another caller may have refreshed the cache while this one waited.
	if entry, ok := c.fresh(); ok {
		return entry.health, entry.err
	}

	health, err := c.probe(ctx)

	c.mu.Lock()
	if !c.coldReported && c.coldStart > 0 {
		// The first probe after a spawn carries the cold-start cost that
		// request paid. Later probes are warm and say so by reporting zero.
		health.ColdStart = c.coldStart
		c.coldReported = true
	}
	c.cached, c.cachedAt = healthEntry{health: health, err: err}, time.Now()
	c.mu.Unlock()

	return health, err
}

// healthEntry is one cached probe. A failed probe is cached too: an
// unreachable source that is asked once per query would otherwise be asked once
// per query.
type healthEntry struct {
	health recall.Health
	err    error
}

func (c *Conn) fresh() (healthEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedAt.IsZero() || time.Since(c.cachedAt) >= c.healthTTL {
		return healthEntry{}, false
	}
	return c.cached, true
}

func (c *Conn) probe(ctx context.Context) (recall.Health, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultProbeTimeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	var health recall.Health
	if err := c.client.Call(ctx, protocol.MethodHealth, protocol.HealthParams{Deadline: deadline.UTC()}, &health); err != nil {
		c.noteFailure(err)
		return Unhealthy(err), fmt.Errorf("%s: health: %w", c.name, err)
	}
	if health.CheckedAt.IsZero() {
		health.CheckedAt = time.Now().UTC()
	}
	if diag := c.diag.Map(); diag != nil {
		if health.Diagnostics == nil {
			health.Diagnostics = make(map[string]any, len(diag))
		}
		for k, v := range diag {
			// Adapter-supplied diagnostics win: they describe the source, and
			// the capture describes the process.
			if _, exists := health.Diagnostics[k]; !exists {
				health.Diagnostics[k] = v
			}
		}
	}
	return health, nil
}

// noteFailure escalates a deadline the adapter never acknowledged. An adapter
// that answered is alive and keeps its process; one that did not is wedged, and
// the supervisor signals it.
func (c *Conn) noteFailure(err error) {
	if c.escalate == nil {
		return
	}
	var timeout *protocol.CallTimeout
	if errors.As(err, &timeout) && !timeout.Acknowledged {
		c.escalate("deadline exceeded without an answer to recall/cancel")
	}
}

// Close asks for a clean exit and releases the stream.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.termGrace)
		defer cancel()
		if err := c.client.Call(ctx, protocol.MethodShutdown, protocol.ShutdownParams{}, nil); err != nil {
			c.closeErr = err
		}
		if err := c.client.Close(); err != nil && c.closeErr == nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}

var _ Adapter = (*Conn)(nil)
