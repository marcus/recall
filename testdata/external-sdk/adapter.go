// Package sdkcheck is a minimal out-of-tree Recall adapter.
package sdkcheck

import (
	"context"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Notes implements the public adapter contract.
type Notes struct{}

// Initialize negotiates the protocol and declares this adapter's capabilities.
func (Notes) Initialize(_ context.Context, cfg adapter.Config) (recall.Manifest, error) {
	version, err := protocol.NegotiateVersion(cfg.ProtocolVersionMin, cfg.ProtocolVersionMax)
	if err != nil {
		return recall.Manifest{}, err
	}
	return recall.Manifest{
		ProtocolVersion: version,
		AdapterID:       "example-notes/1",
		DisplayName:     "Example Notes",
		RecordTypes:     []recall.RecordType{recall.RecordDocument},
		QueryModes:      []recall.QueryMode{recall.QueryLexical},
		FreshnessModes:  []recall.FreshnessMode{recall.FreshnessLive},
		AsOfSupport:     recall.AsOfNone,
		RelevanceBasis:  recall.RelevanceLexicalSpan,
		Capabilities:    []recall.Capability{recall.CapSearch, recall.CapExpand},
		Sensitivity:     recall.SensitivityInternal,
	}, nil
}

// Search reports a complete empty source boundary for the fixture.
func (Notes) Search(context.Context, recall.SearchRequest) (recall.SearchResponse, error) {
	return recall.SearchResponse{Candidates: []recall.Candidate{}, Outcome: recall.SearchSuccess}, nil
}

// Expand reports that the fixture has no locators.
func (Notes) Expand(context.Context, recall.ExpandRequest) (recall.ExpandResponse, error) {
	return recall.ExpandResponse{}, protocol.ErrLocatorUnknown
}

// Health reports the fixture as complete and ready.
func (Notes) Health(context.Context) (recall.Health, error) {
	return recall.Health{Status: recall.HealthHealthy, Coverage: recall.IndexComplete}, nil
}

// Refresh reports the unchanged live source health.
func (n Notes) Refresh(ctx context.Context, _ protocol.RefreshParams) (recall.Health, error) {
	return n.Health(ctx)
}

// Close releases the adapter.
func (Notes) Close() error { return nil }

var _ adapter.Adapter = Notes{}
