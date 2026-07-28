package evidence

import (
	"unicode/utf8"

	"github.com/marcus/recall/internal/recall"
)

// TokenEstimator approximates how much of a response budget some text costs.
//
// Accuracy is not the requirement; determinism is. Shaping decisions appear in
// evaluation runs, so the same input and budget must always produce the same
// response, whatever tokenizer a downstream model happens to use.
type TokenEstimator func(string) int

// EstimateTokens is the default heuristic: roughly four characters per token,
// rounded up, with a floor of one token for any non-empty string.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	runes := utf8.RuneCountInString(s)
	return (runes + 3) / 4
}

// Shaped is a response fitted to a budget.
type Shaped struct {
	// Response is the response as it fits: its results, and a frame whose
	// diagnostics may have been summarized to make room for them.
	Response recall.QueryResponse

	// Tokens is what that response costs on the surface that priced it, frame
	// included. It is what the budget was spent on, not a measurement of the
	// bytes a terminal received.
	Tokens int
}

// Shape fits a response to its budget in one pass: the frame, then three
// phases over the results.
//
// The frame is charged first — the outcome, the coverage line, and whatever
// footers the surface always prints are part of the response, so they are
// inside the budget rather than exempt from it. A frame that does not fit is
// summarized rather than waived: the per-source ledger collapses to
// [recall.SourceSummary] and the plan's source list is dropped, both named in
// [recall.QueryResponse.Omitted]. Results are then fitted into what remains —
// leading ones keeping their excerpts, the rest compressing to a title and
// locator, and the tail dropped and reported.
//
// What is never traded away is the minimal floor: the outcome, the coverage,
// every degraded source by name, and every suppression. Those are claims about
// the evidence, and a response that dropped one to save tokens would be cheaper
// by being less true. On a large profile the floor is tens of tokens, so a
// budget below it is a request nothing could have satisfied.
//
// A budget of zero or less is unbounded. That is the library contract and not
// the product one: a caller holding the struct pays no rendering cost, while
// every surface that prints one substitutes [recall.DefaultResponseTokens] for
// an unset budget before the request arrives here. A caller that wants nothing
// asks for no results, which is a different request.
//
// Truncation is a budget fact and not a coverage fact: every eligible source
// was still searched, so this never sets degraded coverage. It is not an
// outcome fact either — what the corpus said is decided before shaping, so a
// budget too small for one result cannot turn an answer into an abstention.
func Shape(resp recall.QueryResponse, budget recall.Budget, cost Cost) Shaped {
	if cost == nil {
		cost = StructuredCost{}
	}
	results := resp.Results
	full := untrimmed(resp)

	if budget.ResponseTokens <= 0 {
		out := Shaped{Response: full, Tokens: cost.Frame(frame(full))}
		for i, r := range results {
			out.Tokens += cost.Result(i+1, r)
		}
		return out
	}

	shaped, tokens := full, cost.Frame(frame(full))
	if starved(tokens, budget.ResponseTokens, results, cost) {
		// The frame is over budget, or keeping it whole would answer nothing.
		// Its diagnostics are the part that grows with the profile rather than
		// with the answer, so they summarize; what is left is the floor.
		if trimmed, ok := trim(resp); ok {
			if t := cost.Frame(frame(trimmed)); t < tokens {
				shaped, tokens = trimmed, t
			}
		}
	}

	shaped.Results = make([]recall.Result, 0, len(results))
	withExcerpts := true

	for i, r := range results {
		rank := len(shaped.Results) + 1
		if withExcerpts {
			if whole := cost.Result(rank, r); tokens+whole <= budget.ResponseTokens {
				tokens += whole
				shaped.Results = append(shaped.Results, r)
				continue
			}
			// The excerpt budget is spent. Everything from here on compresses,
			// including this result.
			withExcerpts = false
		}

		compressed := compress(r)
		if line := cost.Result(rank, compressed); tokens+line <= budget.ResponseTokens {
			tokens += line
			shaped.Results = append(shaped.Results, compressed)
			continue
		}

		shaped.Truncated = true
		shaped.DroppedResults += len(results) - i
		break
	}
	shaped.Suppressed = carried(shaped.Suppressed, shaped.Results)
	return Shaped{Response: shaped, Tokens: tokens}
}

// carried drops the suppressions whose subject the shaped response no longer
// holds.
//
// Only [recall.SuppressDuplicateView] can be one. It reports a view of a record
// that is in the answer, named by FusedInto, so a budget that dropped that
// result leaves it claiming a second view of something the caller never
// received — the one shape of suppression that would read as more evidence
// withheld than there was. Every other reason names a record withheld outright,
// which stays true however the response was fitted, and none of them is dropped
// here.
//
// It runs after the budget rather than before it, so a response is charged for
// the lines the frame held when it was priced and prints no more than that.
// Reconciling first would need the result set the reconciliation depends on.
func carried(suppressed []recall.Suppression, results []recall.Result) []recall.Suppression {
	shown := make(map[recall.LineageRoot]bool, len(results))
	for _, r := range results {
		shown[r.Explanation.LineageRoot] = true
	}

	keep := func(s recall.Suppression) bool {
		return s.Reason != recall.SuppressDuplicateView || shown[s.FusedInto]
	}
	for _, s := range suppressed {
		if keep(s) {
			continue
		}
		// Copied rather than filtered in place: the slice is the caller's, and
		// the unshaped response is still a legitimate view of the same run.
		out := make([]recall.Suppression, 0, len(suppressed))
		for _, s := range suppressed {
			if keep(s) {
				out = append(out, s)
			}
		}
		return out
	}
	return suppressed
}

// starved reports whether the frame has to give way: it does not fit at all, or
// keeping it whole would leave no room for even one compressed result.
//
// The second case is the one worth stating. A response that spent its entire
// budget on diagnostics and answered nothing is the failure this budget exists
// to prevent, and the caller asked a question — the per-source ledger is how it
// was answered, not the answer.
func starved(frameTokens, budget int, results []recall.Result, cost Cost) bool {
	if frameTokens > budget {
		return true
	}
	if len(results) == 0 {
		return false
	}
	return frameTokens+cost.Result(1, compress(results[0])) > budget
}

// untrimmed is the response as it stands when the whole frame fits: the ledger
// is there, so the summary standing in for it would be the same facts twice.
func untrimmed(resp recall.QueryResponse) recall.QueryResponse {
	resp.SourceSummary = nil
	return resp
}

// trim summarizes the frame's diagnostics, reporting whether there was anything
// to summarize.
//
// The two things it removes are the two that grow with the profile rather than
// with the answer: the per-source ledger, which the summary replaces fact for
// fact except for freshness evidence `recall sources` answers on demand, and
// the plan's source list. The plan's header stays because it is one line and
// describes the request.
//
// Both are named in Omitted. An omission a caller can see is a budget fact; the
// same omission unnamed would read as a source that was never asked.
func trim(resp recall.QueryResponse) (recall.QueryResponse, bool) {
	if len(resp.SourceOutcomes) == 0 && len(resp.Plan.Sources) == 0 {
		return resp, false
	}
	if len(resp.SourceOutcomes) > 0 {
		if resp.SourceSummary == nil {
			// A caller that built the response without one gets the ledger
			// kept: dropping it for a summary that does not exist would lose
			// the degraded sources outright.
			return resp, false
		}
		resp.SourceOutcomes = nil
		resp.Omitted = append(resp.Omitted, recall.OmittedSourceOutcomes)
	} else {
		resp.SourceSummary = nil
	}
	if len(resp.Plan.Sources) > 0 {
		resp.Plan.Sources = nil
		resp.Omitted = append(resp.Omitted, recall.OmittedPlanSources)
	}
	return resp, true
}

// frame is the response minus its results, priced as if shaping will truncate.
//
// Charging the truncated form either way costs a line the response may not
// print, and keeps the frame's own report of what was dropped inside the budget
// rather than overrunning it by exactly the amount the budget caused.
func frame(resp recall.QueryResponse) recall.QueryResponse {
	resp.Truncated = true
	if resp.DroppedResults == 0 {
		resp.DroppedResults = len(resp.Results)
	}
	resp.Results = nil
	return resp
}

// compress strips a result to what a caller needs in order to ask for more:
// a label and a locator.
func compress(r recall.Result) recall.Result {
	r.Primary.Excerpt = ""
	// The kind described an excerpt that is no longer there. Keeping it would
	// claim a match the caller was never shown.
	r.Primary.ExcerptKind = ""
	r.Members = nil
	return r
}
