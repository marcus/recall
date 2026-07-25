package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Problem is an error as a surface reports it: a code from a closed vocabulary
// and a message Recall itself wrote.
//
// The message is never an adapter's error text. Adapter prose is
// source-influenced — it is written by the same untrusted material the adapter
// indexes — and a denied expansion's diagnostics must not reveal whether a
// record exists. A caller that needs the adapter's own words asks
// `recall doctor` or the sources listing, where that text is presented as what
// it is: a report from a source, not a statement by Recall.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p Problem) Error() string { return p.Code + ": " + p.Message }

// Problem codes. The source-failure half of this vocabulary is the same closed
// set [recall.SourceReport] uses for its Reason, so a failed expansion and a
// failed search name their failure the same way. The request half names faults
// in the call itself, which are the caller's to fix.
const (
	// CodeBadRequest means the request could not be read: bad JSON, an unknown
	// field, an argument outside its vocabulary.
	CodeBadRequest = "bad_request"
	// CodeMalformedLocator means the locator is not a source-scoped reference.
	CodeMalformedLocator = "malformed_locator"
	// CodeProfileMismatch means the request named a profile this surface does
	// not serve. See [Core.Profile].
	CodeProfileMismatch = "profile_mismatch"
	// CodeNotFound means there is no such endpoint or tool.
	CodeNotFound = "not_found"
	// CodeOriginRejected means the request carried an Origin header. See
	// guard.go.
	CodeOriginRejected = "origin_rejected"
	// CodeUnauthorized means the server required a bearer token and the caller
	// did not present the configured value.
	CodeUnauthorized = "unauthorized"

	// CodeDenied means the profile's ceiling refuses the source. It says
	// nothing about whether the record exists.
	CodeDenied = "denied"
	// CodeNotConfigured means the locator names a source this machine does not
	// have. That is a fact about this installation, not about the record.
	CodeNotConfigured = "not_configured"
	// CodeUnknownLocator means the owning adapter cannot interpret the local
	// part.
	CodeUnknownLocator = "locator_unknown"
	// CodeExpiredLocator means the source changed incompatibly. Expansion fails
	// rather than returning a different revision or a nearby record.
	CodeExpiredLocator = "locator_expired"
	// CodeUnreachable means the source could not be contacted.
	CodeUnreachable = "unreachable"
	// CodeTimeout means the request ran out of time.
	CodeTimeout = "timeout"
	// CodeBudgetExceeded means the source declined the request up front: it
	// cannot be served within the budget offered.
	CodeBudgetExceeded = "budget_exceeded"
	// CodeFailed means the source could not produce the evidence, for a reason
	// outside the rest of this vocabulary.
	CodeFailed = "failed"
	// CodeInternal means Recall itself failed. It is never used to describe a
	// source.
	CodeInternal = "internal"
)

// classify names a failure in the closed vocabulary, so a caller can act on it
// without reading prose and so no adapter's own error text escapes.
func classify(err error) Problem {
	switch {
	case errors.Is(err, recall.ErrMalformedLocator):
		return Problem{CodeMalformedLocator, "the locator is not a source-scoped reference of the form <source_id>:<local>"}
	case errors.Is(err, protocol.ErrSourceDenied):
		return Problem{CodeDenied, "the active profile's sensitivity ceiling does not permit this source"}
	case errors.Is(err, protocol.ErrSourceNotConfigured):
		return Problem{CodeNotConfigured, "the locator names a source this installation does not have configured"}
	case errors.Is(err, protocol.ErrLocatorUnknown):
		return Problem{CodeUnknownLocator, "the source does not recognize this locator"}
	case errors.Is(err, protocol.ErrLocatorExpired):
		return Problem{CodeExpiredLocator, "the source changed incompatibly and this locator no longer resolves"}
	case errors.Is(err, protocol.ErrSourceUnavailable):
		return Problem{CodeUnreachable, "the source could not be reached"}
	case errors.Is(err, protocol.ErrBudgetExceeded):
		return Problem{CodeBudgetExceeded, "the source declined the request within the budget offered"}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, protocol.ErrDeadlineExceeded):
		return Problem{CodeTimeout, "the source ran out of time"}
	case errors.Is(err, context.Canceled):
		return Problem{CodeTimeout, "the request was cancelled before the source answered"}
	default:
		return Problem{CodeFailed, "the source could not produce the evidence"}
	}
}

// normalizeQuery fills a request's defaults and refuses one this surface cannot
// honor.
//
// Every check here is about refusing rather than repairing. A mode outside the
// vocabulary, or a profile this surface does not serve, would otherwise be
// silently reinterpreted as something the caller did not ask for — and the
// caller would receive a plausible answer to a different question. Empty query
// text is the same failure in its most obvious form.
func normalizeQuery(req *recall.QueryRequest, profile string) *Problem {
	if req.Query == "" {
		return &Problem{CodeBadRequest, "query text is required"}
	}
	if p := checkProfile(req.Profile, profile); p != nil {
		return p
	}
	req.Profile = profile

	switch req.Mode {
	case "":
		// Explicit is the default because it is what a caller asking a question
		// is doing. Pre-reply is a host's speculative retrieval and changes how
		// lineage suppression applies, so it has to be asked for by name.
		req.Mode = recall.ModeExplicit
	case recall.ModeExplicit, recall.ModePreReply:
	default:
		return &Problem{CodeBadRequest, fmt.Sprintf("mode %q: want %q or %q", req.Mode, recall.ModeExplicit, recall.ModePreReply)}
	}
	if req.Limit < 0 {
		return &Problem{CodeBadRequest, "limit must not be negative"}
	}
	return nil
}

// normalizeExpand fills an expansion's defaults and refuses a locator that
// could not round-trip.
//
// The locator check is the round-trip guarantee made explicit: what a query
// returned must be what an expansion accepts. A locator with an empty local
// part is not something this surface ever emitted, so accepting it would mean
// inventing a reference no result carried.
func normalizeExpand(req *recall.ExpandRequest) *Problem {
	if req.Locator.Local == "" {
		return &Problem{CodeMalformedLocator, "a locator needs a local part: <source_id>:<local>, exactly as a query returned it"}
	}
	if req.Locator.SourceID == "" && req.Locator.SourceUID == "" {
		return &Problem{CodeMalformedLocator, "a locator needs a source: <source_id>:<local>, exactly as a query returned it"}
	}
	switch req.Detail {
	case "":
		req.Detail = recall.DetailExcerpt
	case recall.DetailSummary, recall.DetailExcerpt, recall.DetailFull, recall.DetailContext:
	default:
		return &Problem{CodeBadRequest, fmt.Sprintf("detail %q: want summary, excerpt, full, or context", req.Detail)}
	}
	if req.Budget < 0 {
		return &Problem{CodeBadRequest, "budget must not be negative"}
	}
	return nil
}

// checkProfile refuses a request for a profile this surface does not serve.
func checkProfile(want, served string) *Problem {
	if want == "" || want == served {
		return nil
	}
	return &Problem{CodeProfileMismatch, fmt.Sprintf(
		"this surface serves profile %q; start another one for %q rather than mixing them",
		served, want)}
}
