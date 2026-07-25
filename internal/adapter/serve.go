package adapter

import (
	"context"
	"io"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Serve exposes an [Adapter] on a stream, speaking the same protocol an
// external adapter does.
//
// This is the other half of "one contract, two transports". A built-in adapter
// written once is reachable in process through the interface and over the wire
// through here, so conformance transcripts and the evaluation replay exercise
// the same implementation the CLI does — rather than a second one written to
// match.
func Serve(ctx context.Context, r io.Reader, w io.Writer, a Adapter) error {
	return protocol.Serve(ctx, r, w, bridge{adapter: a})
}

// bridge adapts the interface to the wire handler. The only asymmetry is
// shutdown: the protocol asks for a clean exit, which for an adapter means
// releasing whatever it holds.
type bridge struct{ adapter Adapter }

func (b bridge) Initialize(ctx context.Context, p protocol.InitializeParams) (recall.Manifest, error) {
	return b.adapter.Initialize(ctx, p)
}

func (b bridge) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	return b.adapter.Search(ctx, req)
}

func (b bridge) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	return b.adapter.Expand(ctx, req)
}

func (b bridge) Health(ctx context.Context) (recall.Health, error) {
	return b.adapter.Health(ctx)
}

func (b bridge) Shutdown(context.Context) error {
	return b.adapter.Close()
}

var _ protocol.Handler = bridge{}
