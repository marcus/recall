package adapter

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Adapter is Recall's one contract with a source implementation.
//
// Built-in adapters implement it directly; external adapters implement it over
// JSON-RPC. There is one contract with two transports, which is why the
// signatures speak only in domain types: nothing here is shaped by the fact
// that a subprocess might be involved.
type Adapter interface {
	// Initialize negotiates the protocol version and returns what the adapter
	// can do. Negotiation happens once per instance: a range the adapter
	// cannot satisfy fails here rather than degrading to a version neither
	// side implements.
	Initialize(ctx context.Context, cfg Config) (recall.Manifest, error)

	// Search returns this source's own ranked candidates. A source that could
	// not answer reports it in [recall.SearchResponse.Outcome] and returns an
	// error; it never reports success with no candidates.
	Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error)

	// Expand retrieves evidence behind a locator. A source that changed
	// incompatibly fails with locator_expired rather than returning a
	// different revision or a nearby record.
	Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error)

	// Health probes the source. Results may be served from a cache with a TTL;
	// see [DefaultHealthTTL].
	Health(ctx context.Context) (recall.Health, error)

	// Close releases the adapter. For a subprocess this asks for a clean exit
	// and then guarantees one.
	Close() error
}

// Config is the handshake input: protocol version range, writable workdir,
// location, and the adapter-owned settings block.
//
// It is an alias, not a copy, of the wire type. The handshake shape is the
// same fact whether it crosses a process boundary or not, and two structs that
// had to be kept in sync would eventually drift.
type Config = protocol.InitializeParams

// Defaults for adapter supervision.
const (
	// DefaultHealthTTL is how long a health probe is reused. The spec's
	// default; probing every source on every query would cost more than the
	// query.
	DefaultHealthTTL = 30 * time.Second

	// DefaultCancelGrace is how long the advisory cancel notification has to be
	// answered before the process is treated as wedged.
	DefaultCancelGrace = protocol.DefaultCancelGrace

	// DefaultTermGrace is how long a clean exit or a SIGTERM has before SIGKILL.
	DefaultTermGrace = 2 * time.Second

	// DefaultHandshakeTimeout bounds spawn plus initialize. Cold start counts
	// against the request budget, so it cannot be unbounded.
	DefaultHandshakeTimeout = 10 * time.Second

	// DefaultProbeTimeout bounds a health probe whose caller supplied no
	// deadline.
	DefaultProbeTimeout = 5 * time.Second
)

// ErrClosed reports use of an adapter that has been closed.
var ErrClosed = errors.New("adapter: closed")

// Options tune transport behavior. The zero value uses the defaults above.
type Options struct {
	// HealthTTL bounds how long a probe result is reused.
	HealthTTL time.Duration

	// CancelGrace bounds the wait for an answer to recall/cancel before the
	// adapter is treated as wedged.
	CancelGrace time.Duration

	// TermGrace bounds a clean exit, and then bounds SIGTERM before SIGKILL.
	TermGrace time.Duration

	// HandshakeTimeout bounds spawn plus initialize.
	HandshakeTimeout time.Duration

	// Diagnostics receives the adapter's stderr and any protocol violations. A
	// nil value gets a fresh buffer.
	Diagnostics *protocol.Diagnostics

	// MaxFrame lowers the protocol frame limit.
	MaxFrame int
}

func (o Options) healthTTL() time.Duration {
	if o.HealthTTL > 0 {
		return o.HealthTTL
	}
	return DefaultHealthTTL
}

func (o Options) termGrace() time.Duration {
	if o.TermGrace > 0 {
		return o.TermGrace
	}
	return DefaultTermGrace
}

func (o Options) handshakeTimeout() time.Duration {
	if o.HandshakeTimeout > 0 {
		return o.HandshakeTimeout
	}
	return DefaultHandshakeTimeout
}

// Spec is everything needed to run an external adapter.
//
// Command, Args, and Env come only from user-level configuration. A project
// configuration travels with a cloned repository, so it may never introduce an
// executable path; see the trust boundary in docs/spec.md.
type Spec struct {
	// Name identifies this source instance in diagnostics. It is not an
	// identity: source_uid comes from configuration.
	Name string

	Command string
	Args    []string

	// Env replaces the environment when non-nil. A nil value inherits.
	Env []string

	// Dir is the process working directory. It is not the adapter's workdir;
	// that is Config.Workdir, which is where an index may be written.
	Dir string

	Config Config

	Options
}

// FailedSearch renders a failure as an honest search response.
//
// This is the only bridge from an error to a [recall.SearchResponse], and it
// cannot produce [recall.SearchSuccess]. That is deliberate: a missing
// dependency, an unreachable source, or an adapter crash reported as a
// successful empty result would be indistinguishable from a source that simply
// had no matches, and the whole ranking layer downstream would believe it.
func FailedSearch(err error) recall.SearchResponse {
	outcome, reason := Classify(err)
	return recall.SearchResponse{
		Candidates:  nil,
		Outcome:     outcome,
		Diagnostics: map[string]any{"reason": reason},
	}
}

// Unhealthy renders a failed probe. An unreachable source is never healthy and
// never has a known coverage.
func Unhealthy(err error) recall.Health {
	status := recall.HealthUnavailable
	if errors.Is(err, protocol.ErrSourceDenied) {
		status = recall.HealthDenied
	}
	_, reason := Classify(err)
	return recall.Health{
		Status:      status,
		CheckedAt:   time.Now().UTC(),
		Coverage:    recall.IndexUnknown,
		Diagnostics: map[string]any{"reason": reason},
	}
}

// Classify maps a failure to the outcome the core reports for it, plus a short
// machine-readable reason.
//
// The reasons are contract vocabulary, not prose: they appear in
// [recall.SourceReport.Reason], which a person acts on.
func Classify(err error) (recall.SearchOutcome, string) {
	switch {
	case err == nil:
		// Never reached through FailedSearch, but a nil error must not become
		// a success here either — the caller asked for a failure rendering.
		return recall.SearchFailed, "unknown"

	case isTimeout(err):
		return recall.SearchTimeout, timeoutReason(err)

	case errors.Is(err, protocol.ErrSourceUnavailable),
		errors.Is(err, protocol.ErrClosed),
		errors.Is(err, ErrClosed),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrClosedPipe):
		return recall.SearchUnavailable, "unreachable"

	case errors.Is(err, protocol.ErrSourceDenied):
		return recall.SearchDenied, "denied"

	case errors.Is(err, protocol.ErrAsOfUnsupported):
		return recall.SearchFailed, "as_of_unsupported"

	case errors.Is(err, protocol.ErrBudgetExceeded):
		return recall.SearchFailed, "budget_exceeded"

	case errors.Is(err, protocol.ErrSourceNotConfigured):
		return recall.SearchFailed, "source_not_configured"

	case errors.Is(err, protocol.ErrLocatorExpired):
		return recall.SearchFailed, "locator_expired"

	case errors.Is(err, protocol.ErrLocatorUnknown):
		return recall.SearchFailed, "locator_unknown"
	}

	var version *protocol.VersionError
	if errors.As(err, &version) {
		return recall.SearchUnavailable, "protocol_version_unsupported"
	}
	var spawn *SpawnError
	if errors.As(err, &spawn) {
		return recall.SearchUnavailable, "spawn_failed"
	}
	return recall.SearchFailed, "adapter_error"
}

func isTimeout(err error) bool {
	var timeout *protocol.CallTimeout
	if errors.As(err, &timeout) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func timeoutReason(err error) string {
	var timeout *protocol.CallTimeout
	if errors.As(err, &timeout) && !timeout.Acknowledged {
		// The adapter did not answer the cancel at all, which is why the
		// process was signalled. Saying so distinguishes a slow source from a
		// broken one.
		return "deadline_exceeded_unresponsive"
	}
	return "deadline_exceeded"
}

// knownOutcome guards against an adapter inventing an outcome. Anything the
// core does not recognize is a failure, never an implicit success.
func knownOutcome(o recall.SearchOutcome) bool {
	switch o {
	case recall.SearchSuccess, recall.SearchPartial, recall.SearchUnavailable,
		recall.SearchDenied, recall.SearchFailed, recall.SearchTimeout, recall.SearchSkipped:
		return true
	default:
		return false
	}
}

// SpawnError reports that an external adapter could not be started. It is
// distinct from a source failure: nothing was ever asked.
type SpawnError struct {
	Name    string
	Command string
	Err     error
}

func (e *SpawnError) Error() string {
	return "adapter " + e.Name + ": cannot start " + e.Command + ": " + e.Err.Error()
}

func (e *SpawnError) Unwrap() error { return e.Err }
