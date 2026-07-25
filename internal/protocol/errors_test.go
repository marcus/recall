package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/protocol"
)

// The codes are the wire contract. Renumbering one silently changes what every
// adapter in every language means, so the numbers and their names are asserted
// literally against docs/adapter-protocol.md.
func TestErrorCodesAreTheDocumentedNumbers(t *testing.T) {
	tests := []struct {
		code     protocol.Code
		number   int
		name     string
		sentinel error
	}{
		{protocol.CodeSourceUnavailable, -32000, "source_unavailable", protocol.ErrSourceUnavailable},
		{protocol.CodeSourceDenied, -32001, "source_denied", protocol.ErrSourceDenied},
		{protocol.CodeLocatorUnknown, -32002, "locator_unknown", protocol.ErrLocatorUnknown},
		{protocol.CodeLocatorExpired, -32003, "locator_expired", protocol.ErrLocatorExpired},
		{protocol.CodeSourceNotConfigured, -32004, "source_not_configured", protocol.ErrSourceNotConfigured},
		{protocol.CodeAsOfUnsupported, -32005, "as_of_unsupported", protocol.ErrAsOfUnsupported},
		{protocol.CodeBudgetExceeded, -32006, "budget_exceeded", protocol.ErrBudgetExceeded},

		{protocol.CodeParse, -32700, "parse_error", protocol.ErrParse},
		{protocol.CodeInvalidRequest, -32600, "invalid_request", protocol.ErrInvalidRequest},
		{protocol.CodeMethodNotFound, -32601, "method_not_found", protocol.ErrMethodNotFound},
		{protocol.CodeInvalidParams, -32602, "invalid_params", protocol.ErrInvalidParams},
		{protocol.CodeInternal, -32603, "internal_error", protocol.ErrInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int(tt.code) != tt.number {
				t.Errorf("code = %d, want %d", int(tt.code), tt.number)
			}
			if tt.code.String() != tt.name {
				t.Errorf("name = %q, want %q", tt.code, tt.name)
			}

			// A code carried across the wire still matches its sentinel, with
			// whatever message the peer attached.
			carried := protocol.Errorf(tt.code, "detail the adapter chose")
			frame, err := json.Marshal(protocol.NewErrorResponse(protocol.NumberID(1), carried))
			if err != nil {
				t.Fatal(err)
			}
			dec := protocol.NewDecoder(bytes.NewReader(append(frame, '\n')))
			msg, err := dec.Decode()
			if err != nil {
				t.Fatal(err)
			}
			if !errors.Is(msg.Error, tt.sentinel) {
				t.Fatalf("decoded %v does not match its sentinel", msg.Error)
			}
			var got *protocol.Error
			if !errors.As(error(msg.Error), &got) || got.Message != "detail the adapter chose" {
				t.Fatalf("errors.As lost the adapter's message: %+v", got)
			}
		})
	}
}

// Codes are the vocabulary a caller branches on. Two different codes must never
// match each other, or a denial would be handled as an outage.
func TestDistinctCodesDoNotMatch(t *testing.T) {
	denied := protocol.Errorf(protocol.CodeSourceDenied, "no")
	if errors.Is(denied, protocol.ErrSourceUnavailable) {
		t.Error("a denial matched an outage")
	}
	if !errors.Is(denied, protocol.ErrSourceDenied) {
		t.Error("a denial did not match its own sentinel")
	}
}

// A sentinel is shared. Attaching data to one must not edit it for everyone
// else, which is why WithData copies.
func TestWithDataDoesNotMutateASentinel(t *testing.T) {
	withData := protocol.ErrSourceDenied.WithData(map[string]any{"scope": "profile"})
	if protocol.ErrSourceDenied.Data != nil {
		t.Fatal("WithData mutated the shared sentinel")
	}
	if !errors.Is(withData, protocol.ErrSourceDenied) {
		t.Error("the copy stopped matching its sentinel")
	}
	if !strings.Contains(string(withData.Data), "profile") {
		t.Errorf("data = %s", withData.Data)
	}
}

// Wrapping a sentinel with fmt.Errorf is the natural way to write an adapter
// error in Go. It used to send the code and drop the detail, so
// "the store no longer holds td-f62256" arrived as "locator_expired".
func TestWrappedSentinelKeepsItsDiagnostic(t *testing.T) {
	err := fmt.Errorf("%w: the store no longer holds td-f62256", protocol.ErrLocatorExpired)

	got := protocol.AsError(err)
	if got.Code != protocol.CodeLocatorExpired {
		t.Errorf("code = %v, want locator_expired preserved", got.Code)
	}
	if !strings.Contains(got.Message, "td-f62256") {
		t.Errorf("message = %q, want the wrapper's detail preserved", got.Message)
	}
	// The code still matches, so callers keep using errors.Is.
	if !errors.Is(got, protocol.ErrLocatorExpired) {
		t.Error("the rebuilt error no longer matches its sentinel")
	}
}

// For a denied source the detail is the leak: anything specific enough to be
// useful confirms the record exists.
func TestDeniedSourceLeaksNothingFromItsWrapper(t *testing.T) {
	err := fmt.Errorf("%w: user lacks access to salary-review-2026.md",
		protocol.ErrSourceDenied)

	got := protocol.AsError(err)
	if got.Code != protocol.CodeSourceDenied {
		t.Errorf("code = %v, want source_denied", got.Code)
	}
	if strings.Contains(got.Message, "salary") {
		t.Errorf("message = %q leaks what was denied", got.Message)
	}
}
