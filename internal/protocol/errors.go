package protocol

import (
	"encoding/json"
	"fmt"
)

// Code is a JSON-RPC error code. The standard codes are the transport's own;
// the -32000..-32007 block is Recall's, in the range JSON-RPC reserves for
// implementation-defined server errors.
type Code int

// Standard JSON-RPC codes.
const (
	CodeParse          Code = -32700
	CodeInvalidRequest Code = -32600
	CodeMethodNotFound Code = -32601
	CodeInvalidParams  Code = -32602
	CodeInternal       Code = -32603
)

// Recall codes. These are the contract in docs/adapter-protocol.md; the
// numbers are wire values and must not be renumbered.
const (
	// CodeSourceUnavailable means the source cannot be reached. It is never
	// reported as a search that succeeded with no matches.
	CodeSourceUnavailable Code = -32000
	// CodeSourceDenied means permission was refused. Its diagnostics must not
	// reveal whether a record exists.
	CodeSourceDenied Code = -32001
	// CodeLocatorUnknown means the locator does not parse for this adapter.
	CodeLocatorUnknown Code = -32002
	// CodeLocatorExpired means the source changed incompatibly. Expansion fails
	// rather than returning a different revision or a nearby record.
	CodeLocatorExpired Code = -32003
	// CodeSourceNotConfigured means the locator names a source this machine
	// does not have.
	CodeSourceNotConfigured Code = -32004
	// CodeAsOfUnsupported means the historical boundary cannot be honored. A
	// source never answers an as_of query from current state instead.
	CodeAsOfUnsupported Code = -32005
	// CodeBudgetExceeded means the adapter declined the request up front: it
	// cannot be served within the budget offered.
	CodeBudgetExceeded Code = -32006
	// CodeDeadlineExceeded means the request ran out of time, or was abandoned
	// by the caller while in flight.
	//
	// It is distinct from budget_exceeded, which was the only code available
	// before and made one condition report two different ways: a timed-out
	// adapter came back as SearchTimeout in process and as SearchFailed over
	// the wire, purely because a different side of the process boundary
	// noticed. Evaluation compares source outcomes exactly, so the same case
	// scored differently depending on the transport.
	CodeDeadlineExceeded Code = -32007
)

var codeNames = map[Code]string{
	CodeParse:               "parse_error",
	CodeInvalidRequest:      "invalid_request",
	CodeMethodNotFound:      "method_not_found",
	CodeInvalidParams:       "invalid_params",
	CodeInternal:            "internal_error",
	CodeSourceUnavailable:   "source_unavailable",
	CodeSourceDenied:        "source_denied",
	CodeLocatorUnknown:      "locator_unknown",
	CodeLocatorExpired:      "locator_expired",
	CodeSourceNotConfigured: "source_not_configured",
	CodeAsOfUnsupported:     "as_of_unsupported",
	CodeBudgetExceeded:      "budget_exceeded",
	CodeDeadlineExceeded:    "deadline_exceeded",
}

// String renders the contract name of a code, so logs and diagnostics carry
// "source_denied" rather than a number a reader has to look up.
func (c Code) String() string {
	if name, ok := codeNames[c]; ok {
		return name
	}
	return fmt.Sprintf("code(%d)", int(c))
}

// Error is a JSON-RPC error object and a Go error.
//
// Matching is by code: every sentinel below is an *Error carrying only a code,
// and [Error.Is] compares codes, so errors.Is(err, ErrSourceDenied) holds for
// any denial regardless of the message the adapter attached. errors.As recovers
// the full value when the caller wants the message or data.
type Error struct {
	Code    Code            `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Sentinels for errors.Is. They carry no message: a code is the contract, a
// message is a diagnostic.
var (
	ErrParse               = &Error{Code: CodeParse, Message: CodeParse.String()}
	ErrInvalidRequest      = &Error{Code: CodeInvalidRequest, Message: CodeInvalidRequest.String()}
	ErrMethodNotFound      = &Error{Code: CodeMethodNotFound, Message: CodeMethodNotFound.String()}
	ErrInvalidParams       = &Error{Code: CodeInvalidParams, Message: CodeInvalidParams.String()}
	ErrInternal            = &Error{Code: CodeInternal, Message: CodeInternal.String()}
	ErrSourceUnavailable   = &Error{Code: CodeSourceUnavailable, Message: CodeSourceUnavailable.String()}
	ErrSourceDenied        = &Error{Code: CodeSourceDenied, Message: CodeSourceDenied.String()}
	ErrLocatorUnknown      = &Error{Code: CodeLocatorUnknown, Message: CodeLocatorUnknown.String()}
	ErrLocatorExpired      = &Error{Code: CodeLocatorExpired, Message: CodeLocatorExpired.String()}
	ErrSourceNotConfigured = &Error{Code: CodeSourceNotConfigured, Message: CodeSourceNotConfigured.String()}
	ErrAsOfUnsupported     = &Error{Code: CodeAsOfUnsupported, Message: CodeAsOfUnsupported.String()}
	ErrBudgetExceeded      = &Error{Code: CodeBudgetExceeded, Message: CodeBudgetExceeded.String()}
	ErrDeadlineExceeded    = &Error{Code: CodeDeadlineExceeded, Message: CodeDeadlineExceeded.String()}
)

func (e *Error) Error() string {
	if e.Message == "" || e.Message == e.Code.String() {
		return e.Code.String()
	}
	return e.Code.String() + ": " + e.Message
}

// Is matches on code alone. Two errors with the same code are the same failure
// as far as a caller's control flow is concerned; the message is for a human.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && other.Code == e.Code
}

// Errorf builds an error with a formatted message. The message is a
// diagnostic: it must stay safe to show, which for a denied source means it
// must not reveal whether a record exists.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithData attaches structured detail. It returns a copy so a sentinel is
// never mutated.
func (e *Error) WithData(v any) *Error {
	raw, err := json.Marshal(v)
	if err != nil {
		return &Error{Code: e.Code, Message: e.Message}
	}
	return &Error{Code: e.Code, Message: e.Message, Data: raw}
}
