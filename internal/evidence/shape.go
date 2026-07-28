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

// Shaper turns an ordered result list into a response that fits a budget.
type Shaper struct {
	// Estimate defaults to EstimateTokens when nil.
	Estimate TokenEstimator

	// PerResultOverhead accounts for the structure around each entry: the
	// locator, rank, and separators a surface renders. Charging it keeps a
	// long tail of one-line entries from overrunning the budget.
	PerResultOverhead int
}

// Shaped is the outcome of fitting results to a budget.
type Shaped struct {
	Results   []recall.Result
	Truncated bool
	Dropped   int
	Tokens    int
}

// Shape fits results to a response budget in one pass, in three phases.
//
// Leading results keep their excerpts. Once the excerpt budget is exhausted the
// remainder compress to a title and locator, which is still enough for a caller
// to expand what it needs. When even those no longer fit, the tail is dropped
// and reported.
//
// Truncation is a budget fact and not a coverage fact: every eligible source
// was still searched, so this never sets degraded coverage.
func (s Shaper) Shape(results []recall.Result, budget recall.Budget) Shaped {
	estimate := s.Estimate
	if estimate == nil {
		estimate = EstimateTokens
	}
	overhead := s.PerResultOverhead
	if overhead == 0 {
		overhead = 8
	}

	// A zero or negative budget means unbounded. A caller that wants nothing
	// asks for no results, which is a different request.
	if budget.ResponseTokens <= 0 {
		out := Shaped{Results: results}
		for _, r := range results {
			out.Tokens += cost(r, estimate, overhead, true)
		}
		return out
	}

	out := Shaped{Results: make([]recall.Result, 0, len(results))}
	withExcerpts := true

	for i, r := range results {
		if withExcerpts {
			if full := cost(r, estimate, overhead, true); out.Tokens+full <= budget.ResponseTokens {
				out.Tokens += full
				out.Results = append(out.Results, r)
				continue
			}
			// The excerpt budget is spent. Everything from here on compresses,
			// including this result.
			withExcerpts = false
		}

		compressed := compress(r)
		if line := cost(compressed, estimate, overhead, false); out.Tokens+line <= budget.ResponseTokens {
			out.Tokens += line
			out.Results = append(out.Results, compressed)
			continue
		}

		out.Truncated = true
		out.Dropped = len(results) - i
		break
	}
	return out
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

func cost(r recall.Result, estimate TokenEstimator, overhead int, withExcerpt bool) int {
	n := overhead + estimate(r.Primary.Title) + estimate(r.Primary.Locator.String())
	if withExcerpt {
		n += estimate(r.Primary.Excerpt)
		for _, m := range r.Members {
			for _, c := range m.Candidates {
				n += estimate(c.Title)
			}
		}
	}
	return n
}
