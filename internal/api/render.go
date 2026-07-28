package api

import (
	"fmt"
	"strings"

	"github.com/marcus/recall/internal/recall"
)

// renderQueryText is the compact form of a response, for a model to read.
//
// It is a projection of the structured response and never an additional fact:
// everything below is read off the same [recall.QueryResponse] that travels in
// structuredContent. What it must not do is omit anything that changes a
// reader's conclusion, so the outcome, the coverage, every degraded source,
// truncation, and every locator are all here. The parts left out — score
// explanations, the resolved plan, cluster members — change how a result ranked
// but not what it says, and a caller that needs them has them in the structure.
func renderQueryText(resp recall.QueryResponse) string {
	var b strings.Builder

	fmt.Fprintf(&b, "outcome=%s coverage=%s results=%d", resp.Outcome, resp.Coverage, len(resp.Results))
	if resp.Truncated {
		fmt.Fprintf(&b, " truncated=true dropped=%d", resp.DroppedResults)
	}
	b.WriteString("\n")

	switch resp.Outcome {
	case recall.OutcomeAnswered:
	case recall.OutcomeFailed:
		b.WriteString("Every source that was asked failed. Nothing searched the corpus, so this is not evidence that nothing matched.\n")
	case recall.OutcomeAbstained:
		b.WriteString("Nothing matched, and at least one source answered. Reporting that nothing was found is supported.\n")
	}
	// The one line that must never be missing: a source that could not answer
	// is named here, not left to be inferred from an absence further down. It
	// survives a budget that removed the ledger, because the summary standing
	// in for the ledger carries the same names.
	if degraded := degradedSources(resp); len(degraded) > 0 {
		fmt.Fprintf(&b, "Degraded coverage — these sources could not answer: %s. Any answer drawn from this is partial.\n",
			strings.Join(degraded, ", "))
	}
	if len(resp.Suppressed) > 0 {
		for _, s := range resp.Suppressed {
			fmt.Fprintf(&b, "Withheld %d result(s): %s.\n", s.Count, s.Reason)
		}
	}

	for i, r := range resp.Results {
		writeResultText(&b, i+1, r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeResultText is one result's text block. It is its own function so the
// response budget can price what a tool result actually delivers: see
// [ToolCost].
func writeResultText(b *strings.Builder, rank int, r recall.Result) {
	fmt.Fprintf(b, "\n%d. %s", rank, r.Primary.Locator.String())
	if r.Primary.Title != "" {
		fmt.Fprintf(b, "  %s", oneLine(r.Primary.Title))
	}
	fmt.Fprintf(b, "\n   source=%s type=%s score=%.4f", r.Explanation.SourceID, r.Primary.RecordType, r.Score)
	if n := r.Explanation.Corroboration.IndependentUnits; n > 1 {
		// Independent records agreeing is worth a reader's attention and is a
		// different thing from one record seen twice. The corroboration count
		// is what distinguishes them; the member count is not, because two
		// members can be two chunks of one document or two views of one
		// record, and this text would then contradict the structure it is a
		// projection of.
		fmt.Fprintf(b, " corroborated_by=%d", n)
	}
	b.WriteString("\n")
	if r.Primary.Excerpt != "" {
		fmt.Fprintf(b, "   %s\n", oneLine(r.Primary.Excerpt))
	}
}

// degradedSources names the sources that could not answer, from the ledger or
// from the summary that stands in for it under a response budget.
func degradedSources(resp recall.QueryResponse) []string {
	if len(resp.SourceOutcomes) == 0 && resp.SourceSummary != nil {
		return resp.SourceSummary.Degraded
	}
	return DegradedSources(resp.SourceOutcomes)
}

// renderEvidenceText puts the provenance first and the content last, so a
// reader sees where text came from before reading it — and so the untrusted
// half is unambiguously the tail rather than something interleaved with
// Recall's own statements.
func renderEvidenceText(loc recall.Locator, resp recall.ExpandResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "locator=%s", loc)
	if resp.Provenance != "" {
		fmt.Fprintf(&b, " provenance=%s", resp.Provenance)
	}
	if resp.SourceRevision != "" {
		fmt.Fprintf(&b, " revision=%s", resp.SourceRevision)
	}
	if resp.Truncated {
		fmt.Fprintf(&b, " truncated=true")
		if resp.TruncationBoundary != "" {
			fmt.Fprintf(&b, " boundary=%s", resp.TruncationBoundary)
		}
	}
	b.WriteString("\n\n")
	b.WriteString(resp.Content)
	return b.String()
}

// oneLine flattens text onto a single line.
//
// Titles and excerpts are source-controlled. They are already sanitized of
// control characters before they reach any surface, but a multi-line excerpt
// would still let a source's own text imitate the shape of the surrounding
// report — a line that looks like Recall stating an outcome. Keeping each one
// on one line means the structure of this rendering is Recall's alone.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
