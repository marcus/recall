package api

import (
	"net/http"

	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/recall"
)

// Response headers that carry the outcome vocabulary out of band.
//
// The body already carries `outcome` and `coverage` as fields, so these are
// redundant for a client that parses JSON. They exist for the client that does
// not: a shell script, a health probe, a proxy rule. The CLI puts the same
// facts in its exit status for the same reason — a caller has to be able to
// tell "nothing matched" from "the sources were down" without reading prose,
// and a surface that only expressed the difference inside a payload would make
// the cheap check the wrong one.
const (
	HeaderOutcome  = "Recall-Outcome"
	HeaderCoverage = "Recall-Coverage"
	HeaderProfile  = "Recall-Profile"
)

// Severity reduces a query response to the one status both surfaces order by.
//
// Outcome and coverage are orthogonal, so anything that has to produce a single
// verdict must decide which one wins. The rule is the CLI's, unchanged: a
// degraded run that abstained is degraded, not abstained, because an abstention
// is a claim about the corpus and an incomplete set of sources does not support
// one. Reproducing that ordering here rather than inventing a second one is the
// whole point of the function existing.
func Severity(resp recall.QueryResponse) Status {
	switch {
	case resp.Outcome == recall.OutcomeFailed:
		return StatusFailed
	case resp.Coverage == recall.CoverageDegraded:
		return StatusDegraded
	default:
		// Answered and abstained are both supported claims about the corpus,
		// and both are ok at this granularity. The distinction between them is
		// not lost — it is in the response's own Outcome field, and in the
		// Recall-Outcome header — because collapsing it here would be the exact
		// flattening this surface exists to avoid.
		return StatusOK
	}
}

// HTTPStatus maps a severity onto an HTTP status code.
//
// The mapping is what keeps the outcome vocabulary alive for a caller that
// looks only at the status line. A 200 carrying an empty result list means
// "nothing matched, and at least one source answered" — a supported claim. A
// source failure never produces that 200; it produces 503, so a caller cannot
// mistake an outage for an empty corpus. That confusion is the single failure
// this whole system is built to prevent, and an HTTP surface that returned 200
// for both would reintroduce it at the transport.
//
// Degraded is 206 rather than 200. The response is genuinely partial: it is
// real content assembled from fewer sources than the profile names, which is
// the closest thing HTTP has to the meaning. It stays in the 2xx range on
// purpose, because the results in it are usable and a client library that
// treats non-2xx as a thrown error should not discard them.
func HTTPStatus(s Status) int {
	switch s {
	case StatusFailed:
		return http.StatusServiceUnavailable
	case StatusDegraded:
		return http.StatusPartialContent
	default:
		return http.StatusOK
	}
}

// ListingStatus reads the severity of a source listing from its source reports.
//
// It exists so [Core] implementations do not each decide what a degraded
// listing is. A source that was probed and cannot serve is degraded; a source
// the configuration excluded is not, because that is the system doing what it
// was told.
func ListingStatus(degraded bool) Status {
	if degraded {
		return StatusDegraded
	}
	return StatusOK
}

// DegradedSources names every source that was eligible and could not answer.
//
// Both surfaces need this list and neither may derive it differently: the HTTP
// header and the MCP text block have to name the same sources the CLI names, or
// a caller comparing two surfaces would conclude coverage means different
// things on each. The rule is the plan's, not this package's — a skipped source
// degrades only when the reason it was skipped degrades.
func DegradedSources(reports []recall.SourceReport) []string {
	return source.DegradedReports(reports)
}
