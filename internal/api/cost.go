package api

import (
	"encoding/json"
	"strings"

	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/recall"
)

// ToolCost prices what an MCP tool result actually delivers.
//
// A tool result is not the response: it is the response as structured content
// AND the text projection of it, inside a JSON-RPC envelope, all of which
// reaches the model's context. Pricing only the structure would leave the text
// uncharged on the one surface whose whole audience is a context window.
//
// The structured half is charged compactly because that is how a JSON-RPC frame
// is written — indented pricing would over-charge by a quarter and buy a caller
// fewer results than it asked for.
type ToolCost struct{}

var _ evidence.Cost = ToolCost{}

// ToolEnvelopeTokens is charged once per tool result for the JSON-RPC framing
// around it — the message, the content array, the text block's own JSON
// escaping — none of which belongs to any one result. It is a stated constant,
// deliberately generous: the envelope is fixed text, and a budget that has to
// be exact about it is measuring the wrong thing.
const ToolEnvelopeTokens = 96

func (ToolCost) Frame(resp recall.QueryResponse) int {
	resp.Results = nil
	return compact(resp) + evidence.EstimateTokens(renderQueryText(resp)) + ToolEnvelopeTokens
}

func (ToolCost) Result(rank int, r recall.Result) int {
	var b strings.Builder
	writeResultText(&b, rank, r)
	return compact(r) + evidence.EstimateTokens(b.String())
}

// compact charges a value as the JSON-RPC frame writes it. A value that cannot
// be marshaled costs nothing rather than failing the request: the response is
// still the caller's answer, and a budget is not the place to discover a broken
// type.
func compact(v any) int {
	body, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return evidence.EstimateTokens(string(body))
}
