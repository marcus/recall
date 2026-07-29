package adapter

import (
	"context"
	"fmt"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Identity is what configuration assigned a source instance. An adapter never
// supplies any of it: the UID is generated at configuration time, the display
// name is the user's, and the sensitivity floor is policy.
type Identity struct {
	// UID is the immutable configured source identity.
	UID recall.SourceUID
	// ID is the configured display and locator prefix.
	ID string
	// Floor is the least restrictive classification the source may return.
	Floor recall.Sensitivity
}

// WithIdentity stamps configuration's identity onto everything an adapter
// returns, and drops candidates an adapter had no right to emit.
//
// This exists as a wrapper rather than a step in the orchestrator because it
// must be unskippable. Three separate contracts — the candidate envelope, the
// locator model, and lineage grouping — are written assuming a candidate
// arrives already carrying its identity, and lineage grouping resolves the
// locator prefix as a display name when the identity is missing. So an adapter
// that returns `{"locator": "tasks:td-f62256"}` while configured as some other
// source would have its evidence grouped under the Tasks lineage root, and the
// printed locator would route a later expansion to Tasks. One source would
// answer as another.
//
// Wrapping closes that by construction: the source part of every locator is
// replaced with the configured identity regardless of what the adapter wrote,
// so a forged prefix cannot survive. Only derived_from is left alone — those
// name other sources by design, and they are resolved against the profile and
// dropped when unknown, which is a claim about lineage rather than about who
// is answering.
func WithIdentity(a Adapter, id Identity) Adapter {
	bound := &identityAdapter{Adapter: a, id: id}
	if _, ok := a.(PreparedSearcher); ok {
		return &preparedIdentityAdapter{identityAdapter: bound}
	}
	return bound
}

type identityAdapter struct {
	Adapter
	id Identity
}

func (a *identityAdapter) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	resp, err := a.Adapter.Search(ctx, req)
	return a.stampResponse(resp, err)
}

func (a *identityAdapter) stampResponse(
	resp recall.SearchResponse,
	err error,
) (recall.SearchResponse, error) {
	if err != nil {
		return resp, err
	}

	kept := make([]recall.Candidate, 0, len(resp.Candidates))
	dropped := 0
	for _, c := range resp.Candidates {
		// A rank below one would score better than the source's own best hit,
		// and a candidate with no local part cannot be expanded. The wire
		// schema rejects both, but a built-in adapter implements the Go
		// interface directly and never passes through it.
		if c.LocalRank < 1 || c.Locator.Local == "" {
			dropped++
			continue
		}
		kept = append(kept, a.stamp(c))
	}

	if dropped > 0 {
		// One malformed candidate degrades its own source and nothing else.
		// Failing the whole request would let a single bad adapter discard
		// every other source's results.
		resp.Outcome = recall.SearchPartial
		if resp.Diagnostics == nil {
			resp.Diagnostics = map[string]any{}
		}
		resp.Diagnostics["dropped_malformed_candidates"] = dropped
	}
	resp.Candidates = kept
	return resp, nil
}

// preparedIdentityAdapter preserves the optional prepared-search seam through
// the mandatory identity wrapper. Candidate stamping remains unskippable on
// both the ordinary and prepared paths.
type preparedIdentityAdapter struct {
	*identityAdapter
}

func (a *preparedIdentityAdapter) PrepareSearch(
	ctx context.Context,
	req recall.SearchRequest,
) (recall.Health, SearchPreparation, error) {
	return a.Adapter.(PreparedSearcher).PrepareSearch(ctx, req)
}

func (a *preparedIdentityAdapter) SearchPrepared(
	ctx context.Context,
	req recall.SearchRequest,
	preparation SearchPreparation,
) (recall.SearchResponse, error) {
	resp, err := a.Adapter.(PreparedSearcher).SearchPrepared(ctx, req, preparation)
	return a.stampResponse(resp, err)
}

// stamp replaces everything a candidate is not entitled to declare about
// itself.
func (a *identityAdapter) stamp(c recall.Candidate) recall.Candidate {
	c.SourceUID = a.id.UID
	c.SourceID = a.id.ID

	// Keep the adapter's local part, discard its prefix.
	c.Locator = recall.Locator{
		SourceID:  a.id.ID,
		SourceUID: a.id.UID,
		Local:     c.Locator.Local,
	}

	// An adapter may classify a record more restrictively than its source,
	// never less.
	c.Sensitivity = a.id.Floor.Raise(c.Sensitivity)
	return c
}

func (a *identityAdapter) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	// An adapter is only ever asked to expand its own records. Routing is the
	// caller's job, and a locator naming another source reaching this adapter
	// is a routing bug worth failing loudly rather than serving.
	if src := req.Locator.SourceUID; src != "" && src != a.id.UID {
		return recall.ExpandResponse{}, fmt.Errorf(
			"%w: locator names source %q, adapter serves %q", protocol.ErrLocatorUnknown, src, a.id.UID)
	}
	req.Locator = recall.Locator{
		SourceID:  a.id.ID,
		SourceUID: a.id.UID,
		Local:     req.Locator.Local,
	}
	return a.Adapter.Expand(ctx, req)
}
