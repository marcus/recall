package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Version is the JSON-RPC version string every frame carries.
const Version = "2.0"

// The protocol version range this build speaks. Negotiation happens once, in
// recall/initialize; an adapter that cannot satisfy the range fails the
// handshake rather than degrading.
const (
	MinVersion = 1
	MaxVersion = 1
)

// Protocol methods. recall/cancel is the only notification; everything else is
// a request that carries a deadline.
const (
	MethodInitialize = "recall/initialize"
	MethodSearch     = "recall/search"
	MethodExpand     = "recall/expand"
	MethodHealth     = "recall/health"
	MethodRefresh    = "recall/refresh"
	MethodCancel     = "recall/cancel"
	MethodShutdown   = "recall/shutdown"
)

// ID is a JSON-RPC correlation identity.
//
// The specification allows a string or a number and requires a response to
// echo the request's id. The raw JSON text is therefore kept verbatim and used
// as the correlation key: nothing reformats an id, so a large integer or an
// unusual string cannot round-trip into a different value and correlate a
// response to the wrong request.
type ID struct {
	raw string
}

// NumberID builds a numeric id. The client numbers its own requests, so this
// is the form the core uses.
func NumberID(n int64) ID { return ID{raw: strconv.FormatInt(n, 10)} }

// StringID builds a string id, for peers that prefer one.
func StringID(s string) ID {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string fails only on invalid UTF-8, which it
		// replaces rather than rejecting; this branch is unreachable.
		return ID{raw: `""`}
	}
	return ID{raw: string(b)}
}

// IsZero reports whether the id names nothing. A message without an id is a
// notification.
func (id ID) IsZero() bool { return id.raw == "" }

// String renders the id as it appears on the wire.
func (id ID) String() string {
	if id.raw == "" {
		return "<none>"
	}
	return id.raw
}

// MarshalJSON preserves the ID's original JSON representation.
func (id ID) MarshalJSON() ([]byte, error) {
	if id.raw == "" {
		return []byte("null"), nil
	}
	return []byte(id.raw), nil
}

// UnmarshalJSON accepts a JSON-RPC string, number, or null ID.
func (id *ID) UnmarshalJSON(b []byte) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return fmt.Errorf("jsonrpc id: %w", err)
	}
	raw := buf.String()
	if raw == "" {
		return fmt.Errorf("jsonrpc id: empty")
	}
	switch {
	case raw == "null":
		id.raw = ""
		return nil
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("jsonrpc id: %w", err)
		}
		// Canonicalize the escaping. "&" and "&" are the same id, and the
		// encoder emits the escaped form, so storing the encoder's spelling
		// keeps the correlation key stable through a decode-encode round trip.
		*id = StringID(s)
		return nil
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("jsonrpc id: %w", err)
		}
	default:
		return fmt.Errorf("jsonrpc id must be a string or a number, got %s", raw)
	}
	id.raw = raw
	return nil
}

// Message is one JSON-RPC frame. Requests, notifications, and responses share
// one struct because the wire does: which one a frame is follows from which
// fields are present, and [Message.Validate] is the single place that decides.
type Message struct {
	// JSONRPC is always [Version].
	JSONRPC string `json:"jsonrpc"`
	// ID correlates a request and response; it is absent on notifications.
	ID *ID `json:"id,omitempty"`
	// Method names a request or notification.
	Method string `json:"method,omitempty"`
	// Params is the method-specific request payload.
	Params json.RawMessage `json:"params,omitempty"`
	// Result is the method-specific success payload.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is the failure payload.
	Error *Error `json:"error,omitempty"`
}

// NewRequest builds a request frame.
func NewRequest(id ID, method string, params json.RawMessage) *Message {
	return &Message{JSONRPC: Version, ID: &id, Method: method, Params: params}
}

// NewNotification builds a frame that expects no response.
func NewNotification(method string, params json.RawMessage) *Message {
	return &Message{JSONRPC: Version, Method: method, Params: params}
}

// NewResult builds a success response.
func NewResult(id ID, result json.RawMessage) *Message {
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	return &Message{JSONRPC: Version, ID: &id, Result: result}
}

// NewErrorResponse builds a failure response.
func NewErrorResponse(id ID, err *Error) *Message {
	return &Message{JSONRPC: Version, ID: &id, Error: err}
}

// IsRequest reports whether the frame expects a response.
func (m *Message) IsRequest() bool { return m.Method != "" && m.ID != nil && !m.ID.IsZero() }

// IsNotification reports whether the frame is fire-and-forget.
func (m *Message) IsNotification() bool { return m.Method != "" && (m.ID == nil || m.ID.IsZero()) }

// IsResponse reports whether the frame answers an earlier request.
func (m *Message) IsResponse() bool { return m.Method == "" && m.ID != nil && !m.ID.IsZero() }

// Validate rejects frames that are syntactically JSON but not JSON-RPC. It runs
// on every decoded frame so no later code has to guess what a half-formed
// message meant.
func (m *Message) Validate() error {
	if m.JSONRPC != Version {
		return Errorf(CodeInvalidRequest, "jsonrpc must be %q", Version)
	}
	switch {
	case m.Method != "":
		if m.Result != nil || m.Error != nil {
			return Errorf(CodeInvalidRequest, "a request carries no result or error")
		}
		return nil
	case m.ID == nil || m.ID.IsZero():
		return Errorf(CodeInvalidRequest, "frame has neither a method nor an id")
	case m.Result != nil && m.Error != nil:
		return Errorf(CodeInvalidRequest, "a response carries a result or an error, not both")
	case m.Result == nil && m.Error == nil:
		return Errorf(CodeInvalidRequest, "a response carries a result or an error")
	}
	return nil
}

// InitializeParams is the handshake input. The version range is the whole point
// of the handshake: an adapter that cannot land inside it fails explicitly.
//
// Workdir is a writable directory under Recall's state directory. An adapter
// writes indexes and checkpoints only there.
type InitializeParams struct {
	// ProtocolVersionMin is the oldest protocol version the caller accepts.
	ProtocolVersionMin int `json:"protocol_version_min"`
	// ProtocolVersionMax is the newest protocol version the caller accepts.
	ProtocolVersionMax int `json:"protocol_version_max"`

	// Workdir is the adapter's writable state directory.
	Workdir string `json:"workdir"`

	// SourceID is the display name this instance was configured under. An
	// adapter needs it because locator text is "<source_id>:<local>" and it
	// writes its own locators. It is NOT identity: the core attaches the
	// immutable source_uid and overwrites the source part of every locator an
	// adapter returns, so a forged prefix cannot make one source answer as
	// another.
	SourceID string `json:"source_id"`

	// Location is the configured path, endpoint, or connection reference for
	// this source instance. Configuration has already resolved a relative one
	// against the file that declared it.
	Location string `json:"location,omitempty"`

	// BaseDir is the directory of the file that declared this source.
	//
	// It exists because Settings is adapter-owned, so configuration cannot
	// resolve paths inside it — an adapter reading settings.root had nothing to
	// resolve against and fell back to the process working directory, which
	// makes the same configuration read different files depending on where
	// Recall was started.
	BaseDir string `json:"base_dir,omitempty"`

	// Settings is the adapter-owned settings block. Its shape is declared by
	// the manifest's settings_schema, which is only knowable after the
	// handshake, so the adapter validates it — the core validates it against
	// the manifest it already holds when it has one.
	Settings map[string]any `json:"settings,omitempty"`
}

// HealthParams carries the probe's deadline. Every request carries one.
type HealthParams struct {
	// Deadline is the absolute time by which the probe must finish.
	Deadline time.Time `json:"deadline"`
}

// RefreshParams asks an adapter to bring its projection up to date and returns
// the resulting health. Only adapters declaring [recall.CapCheckpoint] serve it.
type RefreshParams struct {
	// Deadline is the absolute time by which the refresh must finish.
	Deadline time.Time `json:"deadline"`

	// Full asks for a complete rebuild rather than an incremental pass. An
	// adapter with no incremental path may treat every refresh as full.
	Full bool `json:"full,omitempty"`
}

// CancelParams names the in-flight request to abandon. Cancellation is
// advisory: the core still enforces the deadline itself.
type CancelParams struct {
	// ID names the request to abandon.
	ID ID `json:"id"`
}

// ShutdownParams asks for a clean exit.
type ShutdownParams struct{}

// ShutdownResult acknowledges a clean exit.
type ShutdownResult struct{}

// VersionError reports a handshake that could not be satisfied. It is returned
// rather than silently choosing a version either end does not implement.
type VersionError struct {
	// Min and Max are the requested version range.
	Min, Max int
	// Offered is the version the peer named, zero when it named none.
	Offered int
	// Supported describes what this end can speak.
	SupportedMin, SupportedMax int
}

func (e *VersionError) Error() string {
	if e.Offered != 0 {
		return fmt.Sprintf("protocol version %d is outside the requested range %d..%d",
			e.Offered, e.Min, e.Max)
	}
	return fmt.Sprintf("requested protocol version range %d..%d, this build speaks %d..%d",
		e.Min, e.Max, e.SupportedMin, e.SupportedMax)
}

// NegotiateVersion picks the highest version both ends support. An adapter
// calls it from its initialize handler; a range with no overlap is an error,
// never a downgrade.
func NegotiateVersion(minVersion, maxVersion int) (int, error) {
	if minVersion > maxVersion {
		return 0, &VersionError{Min: minVersion, Max: maxVersion, SupportedMin: MinVersion, SupportedMax: MaxVersion}
	}
	best := 0
	for v := MinVersion; v <= MaxVersion; v++ {
		if v >= minVersion && v <= maxVersion && v > best {
			best = v
		}
	}
	if best == 0 {
		return 0, &VersionError{Min: minVersion, Max: maxVersion, SupportedMin: MinVersion, SupportedMax: MaxVersion}
	}
	return best, nil
}

// CheckNegotiated verifies the version an adapter reported is one the core
// asked for. A manifest naming anything else fails the handshake.
func CheckNegotiated(p InitializeParams, got int) error {
	if got < p.ProtocolVersionMin || got > p.ProtocolVersionMax {
		return &VersionError{
			Min: p.ProtocolVersionMin, Max: p.ProtocolVersionMax, Offered: got,
			SupportedMin: MinVersion, SupportedMax: MaxVersion,
		}
	}
	return nil
}
