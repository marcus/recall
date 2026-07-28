package cli

import (
	"encoding/json"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/pointer"
	"github.com/marcus/recall/internal/recall"
)

// renderCosts prices every response a core built here can be asked to render:
// the CLI's two human tiers, and the MCP tool results this process also serves
// through `recall mcp` and `recall serve`.
//
// The structured surface is absent on purpose: a response emitted as JSON costs
// its own serialization, which is what [evidence.StructuredCost] already
// charges, and a second opinion about that here would be a number to keep in
// step with encoding/json.
func renderCosts() map[recall.ResponseSurface]evidence.Cost {
	return map[recall.ResponseSurface]evidence.Cost{
		recall.SurfacePointer:           humanCost{},
		recall.SurfaceExplained:         humanCost{explained: true},
		recall.SurfaceStructuredPointer: pointerJSONCost{},
		recall.SurfaceTool:              api.ToolCost{},
		recall.SurfaceToolExplained:     api.ToolCost{Explained: true},
	}
}

// pointerJSONCost charges what `recall query --json` serializes, by serializing
// it — the same method humanCost uses, for the same reason. A table of
// constants here would be a copy of pointer.go's field list that nothing forces
// anyone to update.
//
// Without this the surface falls back to [evidence.StructuredCost], which is
// safe and wrong: safe because a projection of a response can only be smaller
// than the whole of it, wrong because the budget would then shape the answer
// for fields this invocation does not print. On the response that motivated the
// projection that is a sevenfold overcharge, and it would spend as truncation —
// a machine caller told "truncated, dropped 7" where a person running the same
// query saw every result.
type pointerJSONCost struct{}

// Frame prices the projected response with no results in it: the outcome, the
// coverage, the source summary, and any suppression or omission. It is exactly
// what this surface emits when nothing fits, so it is the floor no budget can
// go below.
func (pointerJSONCost) Frame(resp recall.QueryResponse) int {
	resp.Results = nil
	return chargeJSON(pointer.Project(resp), "")
}

// Result charges one projected result where it lands: two levels inside the
// response, plus the indent that opens the array element and the separator that
// joins it to the next, neither of which [json.MarshalIndent] produces for the
// value on its own. That is [evidence.StructuredCost.Result]'s arithmetic
// applied to the smaller shape, and what is charged has to be a ceiling on what
// gets written or the budget is a suggestion.
//
// Unlike the structured surface, the rank IS charged: a projected result
// carries it as a field rather than as its position in the array, so at rank
// 100 the number really is two characters wider than at rank 1.
func (pointerJSONCost) Result(rank int, r recall.Result) int {
	return chargeJSON(pointer.ProjectResult(rank, r), resultIndent) + estimateJSON(resultIndent+",\n")
}

// resultIndent is how deep a result sits in a serialized response: inside the
// response object, inside its results array. It matches the constant
// internal/evidence prices the structured surface with; the two surfaces nest
// their results identically, and a projection that pretended otherwise would
// underprice every line of every result.
const resultIndent = "    "

// chargeJSON serializes with the indentation the surface emits, so the charge
// is the size of the bytes written and not of a denser form nobody sends. A
// value that cannot be marshaled costs nothing rather than failing the request:
// the response is still the caller's answer, and a budget is not the place to
// discover a broken type.
func chargeJSON(v any, prefix string) int {
	body, err := json.MarshalIndent(v, prefix, "  ")
	if err != nil {
		return 0
	}
	return estimateJSON(string(body))
}

func estimateJSON(s string) int { return evidence.EstimateTokens(s) }

// humanCost charges what the human renderer prints, by running it.
//
// Rendering the piece and estimating the text is the point: a table of
// per-piece constants here would be a copy of query.go's layout that nothing
// forces anyone to update, and it would go stale the first time a block moves.
// This cannot go stale, and it stays deterministic because the renderer and the
// estimator both are.
type humanCost struct{ explained bool }

// Frame prices the response with no results in it: the outcome line, the
// coverage and suppression lines, and — behind --explain — the source ledger
// and the plan. It is exactly what the surface prints when nothing fits, so it
// is also the floor no budget can go below.
func (c humanCost) Frame(resp recall.QueryResponse) int {
	var o out
	renderQuery(&o, resp, c.explained)
	return evidence.EstimateTokens(o.buf.String())
}

// Result prices one result at the rank it will be printed at, digits included:
// a hundred results are a hundred uncharged characters otherwise, and "never
// over budget" cannot survive a rounding error that only ever goes one way.
func (c humanCost) Result(rank int, r recall.Result) int {
	var o out
	renderResult(&o, rank, r)
	if c.explained {
		renderResultDetail(&o, r)
	}
	return evidence.EstimateTokens(o.buf.String())
}
