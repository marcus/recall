package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/evidence"
	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// DefaultExpandBudget bounds an expansion whose caller stated none.
const DefaultExpandBudget int64 = 64 << 10

// Expand retrieves the evidence behind a locator.
//
// It is stateless with respect to the query that produced the locator: a
// locator printed yesterday expands today, or fails explicitly. Permissions are
// enforced here as well as at query time, because a locator can be held and
// replayed long after the response that carried it, and a profile's ceiling may
// have narrowed in between.
func (a *App) Expand(ctx context.Context, req recall.ExpandRequest, profileName string) (recall.ExpandResponse, error) {
	if req.Locator.Local == "" {
		return recall.ExpandResponse{}, fmt.Errorf("%w: no local part", recall.ErrMalformedLocator)
	}

	inst, err := a.resolveSource(req.Locator)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	profile, err := a.cfg.ActiveProfile(profileName)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	if !profile.Permits(*inst) {
		// Denied without confirming the record exists: saying "expired" or
		// "unknown" here would leak whether it does.
		return recall.ExpandResponse{}, fmt.Errorf("%w: source %q is above the %s ceiling of profile %q",
			protocol.ErrSourceDenied, inst.ID, profile.MaxSensitivity, profile.Name)
	}

	// Handshake first, for the same reason the plan does: a built-in adapter is
	// constructed unconfigured, and an expansion is often the first thing a
	// fresh process does with a locator somebody saved yesterday.
	if _, err := a.registry.Initialize(ctx, inst); err != nil {
		return recall.ExpandResponse{}, err
	}
	adp, err := a.registry.Adapter(inst)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	if req.Budget <= 0 {
		req.Budget = DefaultExpandBudget
	}
	if req.Detail == "" {
		req.Detail = recall.DetailExcerpt
	}
	if req.Deadline.IsZero() {
		req.Deadline = a.now().Add(DefaultExpandTimeout)
	}
	ctx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()

	resp, err := adp.Expand(ctx, req)
	if err != nil {
		return recall.ExpandResponse{}, err
	}

	// Expanded evidence is the largest untrusted payload Recall handles and the
	// one most likely to be pasted into a terminal or handed to a model.
	limits := a.limits
	if req.Budget > 0 && int64(limits.Content) > req.Budget {
		limits.Content = int(req.Budget)
	}
	clean, _ := evidence.SanitizeEvidence(resp, limits)
	return clean, nil
}

// DefaultExpandTimeout bounds an expansion whose caller set no deadline.
const DefaultExpandTimeout = 10 * time.Second

// resolveSource finds the source a locator names, by whichever half of its
// identity it carries.
func (a *App) resolveSource(loc recall.Locator) (*config.SourceInstance, error) {
	resolved, err := lineage.Resolve(a.cfg, loc)
	if err != nil {
		var notConfigured *lineage.ErrNotConfigured
		if errors.As(err, &notConfigured) {
			// A portable locator may name a source configured elsewhere. That
			// is a fact about this machine, so it is reported as itself rather
			// than as a missing record.
			return nil, fmt.Errorf("%w: %w", protocol.ErrSourceNotConfigured, err)
		}
		return nil, err
	}
	inst, ok := a.cfg.Source(resolved.SourceID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", protocol.ErrSourceNotConfigured, resolved.SourceID)
	}
	return inst, nil
}
