package cli

import (
	"testing"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/recall"
)

// Every surface this process renders has to be priced here. `recall mcp` and
// `recall serve` are served by the same core the CLI builds, so a missing entry
// would not fail — it would silently price a tool result as a JSON body it
// never sends, and the budget would bound the wrong thing.
func TestRenderCostsCoverEverySurfaceThisProcessRenders(t *testing.T) {
	costs := renderCosts()

	for _, surface := range []recall.ResponseSurface{
		recall.SurfacePointer, recall.SurfaceExplained, recall.SurfaceTool,
	} {
		if _, ok := costs[surface]; !ok {
			t.Errorf("surface %q has no price registered", surface)
		}
	}
	if _, ok := costs[recall.SurfaceTool].(api.ToolCost); !ok {
		t.Error("the tool surface must be priced by the transport that renders it")
	}
	// The structured surface is priced by its own serialization. A second
	// opinion here would be a number to keep in step with encoding/json.
	if _, ok := costs[recall.SurfaceStructured]; ok {
		t.Error("the structured surface should not be priced here")
	}
}
