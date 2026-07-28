package cli

import (
	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/evidence"
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
		recall.SurfacePointer:   humanCost{},
		recall.SurfaceExplained: humanCost{explained: true},
		recall.SurfaceTool:      api.ToolCost{},
	}
}

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
