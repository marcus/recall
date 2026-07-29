package evidence

import (
	"encoding/json"

	"github.com/marcus/recall/pkg/recall"
)

// Cost prices a response as one surface renders it.
//
// A response budget that charges for fields rather than for output budgets a
// response nobody prints: the same result is a hundred tokens as a pointer and
// closer to two thousand serialized, and the frame around it — the outcome
// line, the source ledger, the plan — is not free either. So the surface that
// will render the response says what it costs, and shaping spends against that.
//
// Implementations render the piece and estimate the text rather than
// approximating it with constants. That is what keeps a charge from drifting
// away from the renderer it charges for, and it stays deterministic because the
// estimator is.
type Cost interface {
	// Frame is what the surface renders regardless of how many results fit:
	// the outcome, whatever coverage and suppression it states, and any footer
	// it always prints. It is charged before any result, so a response whose
	// frame alone exceeds the budget carries no results rather than pretending
	// the frame was free.
	Frame(recall.QueryResponse) int

	// Result is one result as the surface renders it at this rank. The rank is
	// passed because a surface prints it: at rank 100 the number is two
	// characters wider than at rank 1, and a long tail of short results is
	// exactly where uncharged characters accumulate.
	//
	// A result whose excerpt and members have been stripped must cost less, or
	// compression buys the budget nothing.
	Result(rank int, r recall.Result) int
}

// StructuredCost prices the response as its own serialization.
//
// It is the default because a caller that did not project the response
// receives this one, and because every projection of it is smaller: an
// underestimate would let a surface overrun its budget, and this cannot
// underestimate anything derived from it.
type StructuredCost struct {
	// Estimate defaults to EstimateTokens when nil.
	Estimate TokenEstimator
}

func (c StructuredCost) Frame(resp recall.QueryResponse) int {
	resp.Results = nil
	return c.charge(resp, "")
}

// Result charges the result where it lands: two levels inside the response, so
// every line of it carries four more spaces than it would standing alone.
// Charging the unindented form underprices a long result by roughly one token
// per four lines, which is exactly the kind of quiet underestimate this whole
// pricing exists to remove.
//
// The same reasoning covers the last six characters of an array element, which
// [json.MarshalIndent] does not produce for the value on its own: the indent
// that opens it and the separator that joins it to the next one. Six characters
// is nothing per result and adds up to more than a line over a long tail, and
// it was enough to put a serialized response six tokens over a 3,000-token
// budget. What is charged here has to be a ceiling on what gets written, or the
// budget is a suggestion.
//
// The rank is not charged: a serialized result carries its position in the
// array, not as a field.
func (c StructuredCost) Result(_ int, r recall.Result) int {
	body, err := json.MarshalIndent(r, resultIndent, "  ")
	if err != nil {
		return 0
	}
	return c.estimate(resultIndent + string(body) + ",\n")
}

// resultIndent is how deep a result sits in a serialized response: inside the
// response object, inside its results array.
const resultIndent = "    "

// charge serializes with the indentation the surfaces emit, so the charge is
// the size of the bytes that get written and not of a denser form nobody sends.
// A value that cannot be marshaled costs nothing rather than failing the
// request: the response is still the caller's answer, and a budget is not the
// place to discover a broken type.
func (c StructuredCost) charge(v any, prefix string) int {
	body, err := json.MarshalIndent(v, prefix, "  ")
	if err != nil {
		return 0
	}
	return c.estimate(string(body))
}

func (c StructuredCost) estimate(s string) int {
	if c.Estimate == nil {
		return EstimateTokens(s)
	}
	return c.Estimate(s)
}
