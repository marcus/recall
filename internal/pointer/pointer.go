// Package pointer projects a query response down to its pointer tier: what a
// caller needs in order to choose a locator to expand, and nothing that only
// says how the result got there.
//
// It is its own package because three surfaces project the same response and no
// two of them can own the definition. `recall query --json` emits it, the MCP
// tool result carries it as structured content, and the HTTP body carries it on
// request — and the budget prices all three by serializing this shape, so a
// second opinion about which fields survive would be a second answer to what a
// budget buys. It sits below internal/cli and internal/api and above
// pkg/recall, which cannot reach internal/source for the degraded summary
// without a cycle.
package pointer

import (
	"time"

	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/recall"
)

// Response is the pointer tier of a response, serialized: what `recall
// query --json` emits, what the MCP tool returns as structured content, and the
// machine counterpart of what the CLI prints without --explain.
//
// It exists because the parity rule is that the same facts are AVAILABLE from
// each surface, not that every rendering prints all of them — and the argument
// docs/spec.md already makes for projecting the human surface was never applied
// here. Printing everything at equal weight made the caller pay for the whole
// response before reading the first result; that cost is worse in JSON, not
// better. Measured on `recall query dentist`, four results: 22,698 bytes, of
// which the four primaries were 3,226. The rest was the per-source ledger and
// plan (8,478 bytes, unchanged by --limit and identical on a query that found
// nothing) and members[].candidates[] re-serializing each primary verbatim
// (8,247 bytes, for four clusters that were all singletons).
//
// What is dropped is exactly the diagnostic tier: score, explanation, cluster
// members, and the per-result provenance the human surface prints behind
// --explain. What is kept is every fact that is a CLAIM — the outcome, the coverage, which sources
// could not answer, what was suppressed, and what a budget removed. Those are
// the same four exemptions the human surface makes, for the same reason: their
// absence would read as an answer that had nothing more to give.
//
// `--json --explain` remains the complete serialization, byte for byte what
// `--json` alone emitted before this projection existed, so a consumer that
// needs a dropped field has an exact migration rather than a reconstruction.
type Response struct {
	// Tier names this shape so a consumer can tell which it is holding without
	// inferring it from a missing key, and so the complete form stays byte-
	// identical to what it has always been rather than gaining a field.
	Tier string `json:"tier"`

	Results []Result `json:"results"`

	// SourceSummary is unconditional here, where the ledger it stands in for is
	// not. The full ledger is the part that grows with the profile and that
	// `recall sources` answers on demand; the counts and the degraded list are
	// what the ledger is actually read for, and the degraded list is a claim
	// about the evidence rather than a diagnostic. Emitting it always makes
	// this surface state its own completeness more explicitly than the human
	// one does, for about a hundred bytes.
	SourceSummary *recall.SourceSummary `json:"source_summary,omitempty"`

	Suppressed []recall.Suppression `json:"suppressed,omitempty"`

	// Omitted keeps naming what the response BUDGET removed. It never names the
	// fields this projection drops: those are documented, reachable by a flag,
	// and identical on every query, which is the difference between a fact that
	// did not fit and a fact this surface does not carry.
	Omitted []recall.Omission `json:"omitted,omitempty"`

	Outcome  recall.Outcome  `json:"outcome"`
	Coverage recall.Coverage `json:"coverage"`

	Truncated      bool `json:"truncated"`
	DroppedResults int  `json:"dropped_results,omitempty"`

	Elapsed time.Duration `json:"elapsed_ns"`
}

// Result is one result as a pointer: what a caller needs in order to
// decide which locator to expand.
//
// It carries two fields the human tier deliberately leaves out. renderResult
// omits the source because the locator prints directly above it and restating
// it would spend a line saying nothing new; that reasoning does not survive the
// change of reader. A locator is a display string to a person and a parse
// target to a program, and making a consumer split on a separator to learn
// which source answered — or which kind of record it is about to expand — is
// precisely the string surgery a structured surface exists to prevent. Both are
// short, both are already in every candidate, and routing on them is the most
// common thing a machine caller does with a result list.
type Result struct {
	Rank       int               `json:"rank"`
	Locator    recall.Locator    `json:"locator"`
	SourceID   string            `json:"source_id"`
	RecordType recall.RecordType `json:"record_type"`

	Title       string             `json:"title,omitempty"`
	Excerpt     string             `json:"excerpt,omitempty"`
	ExcerptKind recall.ExcerptKind `json:"excerpt_kind,omitempty"`

	// Exact and Corroboration are the two markers the human tier prints, and
	// they are here for the reason they are there: each states something the
	// rank does not. Exact says the query named this record outright, which is
	// what makes a preview excerpt a strong result rather than a suspicious
	// one. Corroboration counts independent records, never cluster members.
	Exact         bool `json:"exact,omitempty"`
	Corroboration int  `json:"corroboration,omitempty"`
}

// Project reduces a response to its pointer tier.
//
// The summary is built here when the budget did not already build one, so this
// surface reports the same source facts whether or not the ledger survived
// shaping. A response that arrived with SourceSummary already standing in for
// the ledger keeps that one: it is the same reduction, and rebuilding it from
// an empty ledger would report zero sources for a request that asked many.
func Project(resp recall.QueryResponse) Response {
	out := Response{
		Tier:           "pointer",
		Results:        make([]Result, 0, len(resp.Results)),
		SourceSummary:  resp.SourceSummary,
		Suppressed:     resp.Suppressed,
		Omitted:        resp.Omitted,
		Outcome:        resp.Outcome,
		Coverage:       resp.Coverage,
		Truncated:      resp.Truncated,
		DroppedResults: resp.DroppedResults,
		Elapsed:        resp.Elapsed,
	}
	if out.SourceSummary == nil {
		out.SourceSummary = summarizeReports(resp.SourceOutcomes)
	}
	for i, r := range resp.Results {
		out.Results = append(out.Results, ProjectResult(i+1, r))
	}
	return out
}

// ProjectResult reduces one result to its pointer form at a stated rank. It is
// exported because the response budget prices a result by serializing exactly
// this, one at a time, as it decides how many fit.
func ProjectResult(rank int, r recall.Result) Result {
	out := Result{
		Rank:        rank,
		Locator:     r.Primary.Locator,
		SourceID:    r.Primary.SourceID,
		RecordType:  r.Primary.RecordType,
		Title:       r.Primary.Title,
		Excerpt:     r.Primary.Excerpt,
		ExcerptKind: r.Primary.ExcerptKind,
		Exact:       r.Explanation.ExactPromoted,
	}
	// Units, not members: a record that arrived as three chunks corroborates
	// nothing, and one is not corroboration at all.
	if n := r.Explanation.Corroboration.IndependentUnits; n > 1 {
		out.Corroboration = n
	}
	return out
}

// summarizeReports reduces the per-source ledger the same way the response
// budget's own summary does. The degraded list is the part that must survive:
// everything else is convenience, and a summary that dropped it would turn an
// incomplete answer into a silent one.
func summarizeReports(reports []recall.SourceReport) *recall.SourceSummary {
	if len(reports) == 0 {
		return nil
	}
	out := recall.SourceSummary{
		Sources:  len(reports),
		Outcomes: make(map[recall.SearchOutcome]int, 4),
		Degraded: source.DegradedReports(reports),
	}
	for _, r := range reports {
		out.Outcomes[r.Outcome]++
	}
	return &out
}
