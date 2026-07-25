package app

import (
	"context"
	"errors"
	"sync"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// Refresh updates one source, or every eligible checkpoint-capable source in
// the selected profile. Sources are reported in profile order even though
// eligible refreshes run concurrently.
func (a *App) Refresh(ctx context.Context, req recall.RefreshRequest) (recall.RefreshResponse, error) {
	started := a.now()
	resp := recall.RefreshResponse{Sources: []recall.RefreshSourceOutcome{}}

	profile, err := a.cfg.ActiveProfile(req.Profile)
	if err != nil {
		return resp, err
	}
	members, err := a.cfg.ProfileSources(profile.Name)
	if err != nil {
		return resp, err
	}

	if req.SourceID != "" {
		inst, configured := a.cfg.Source(req.SourceID)
		switch {
		case !configured:
			resp.Sources = append(resp.Sources, recall.RefreshSourceOutcome{
				SourceID: req.SourceID,
				Status:   recall.RefreshSourceFailed,
				Reason:   recall.RefreshSourceNotConfigured,
			})
			resp.Outcome = recall.RefreshFailed
			resp.Elapsed = a.now().Sub(started)
			return resp, nil
		case !profile.Contains(req.SourceID):
			resp.Sources = append(resp.Sources, recall.RefreshSourceOutcome{
				SourceID:  inst.ID,
				SourceUID: inst.UID,
				Status:    recall.RefreshSourceFailed,
				Reason:    recall.RefreshSourceNotInProfile,
			})
			resp.Outcome = recall.RefreshFailed
			resp.Elapsed = a.now().Sub(started)
			return resp, nil
		default:
			members = []*config.SourceInstance{inst}
		}
	}

	results := make([]recall.RefreshSourceOutcome, len(members))
	eligible := make([]bool, len(members))
	var wg sync.WaitGroup
	for i, inst := range members {
		results[i] = recall.RefreshSourceOutcome{SourceID: inst.ID, SourceUID: inst.UID}
		switch {
		case !inst.Enabled:
			results[i].Status = recall.RefreshSourceSkipped
			results[i].Reason = recall.RefreshDisabled
		case !profile.Permits(*inst):
			results[i].Status = recall.RefreshSourceSkipped
			results[i].Reason = recall.RefreshDenied
		default:
			manifest, initErr := a.registry.Initialize(ctx, inst)
			if initErr != nil {
				results[i].Status = recall.RefreshSourceFailed
				results[i].Reason = classifyRefreshError(initErr, recall.RefreshInitializeFailed)
				eligible[i] = true
				continue
			}
			if !manifest.Can(recall.CapCheckpoint) {
				results[i].Status = recall.RefreshSourceSkipped
				results[i].Reason = recall.RefreshCheckpointUnsupported
				continue
			}
			eligible[i] = true
			wg.Add(1)
			go func(i int, inst *config.SourceInstance) {
				defer wg.Done()
				results[i] = a.refreshOne(ctx, inst, req.Full)
			}(i, inst)
		}
	}
	wg.Wait()

	resp.Sources = results
	resp.Outcome = aggregateRefresh(results, eligible, req.SourceID != "")
	resp.Elapsed = a.now().Sub(started)
	return resp, nil
}

func (a *App) refreshOne(parent context.Context, inst *config.SourceInstance, full bool) recall.RefreshSourceOutcome {
	out := recall.RefreshSourceOutcome{SourceID: inst.ID, SourceUID: inst.UID}
	started := a.now()

	ctx, cancel := context.WithTimeout(parent, inst.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	adp, err := a.registry.Adapter(inst)
	if err != nil {
		out.Status = recall.RefreshSourceFailed
		out.Reason = classifyRefreshError(err, recall.RefreshOperationFailed)
		out.Elapsed = a.now().Sub(started)
		return out
	}
	health, err := adp.Refresh(ctx, protocol.RefreshParams{Deadline: deadline, Full: full})
	out.Elapsed = a.now().Sub(started)
	if health.Status != "" {
		out.Health = &health
	}
	if err != nil {
		out.Status = recall.RefreshSourceFailed
		out.Reason = classifyRefreshError(err, recall.RefreshOperationFailed)
		return out
	}
	switch health.Status {
	case recall.HealthHealthy:
		out.Status = recall.RefreshSourceRefreshed
	case recall.HealthDegraded:
		out.Status = recall.RefreshSourceDegraded
		out.Reason = recall.RefreshUnhealthy
	default:
		out.Status = recall.RefreshSourceFailed
		out.Reason = recall.RefreshUnhealthy
	}
	return out
}

func classifyRefreshError(err error, fallback recall.RefreshReason) recall.RefreshReason {
	switch {
	case errors.Is(err, context.Canceled):
		return recall.RefreshCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, protocol.ErrDeadlineExceeded):
		return recall.RefreshTimedOut
	default:
		return fallback
	}
}

func aggregateRefresh(results []recall.RefreshSourceOutcome, eligible []bool, targeted bool) recall.RefreshOutcome {
	usable, degraded, attempted := 0, false, 0
	for i, result := range results {
		if eligible[i] {
			attempted++
		}
		switch result.Status {
		case recall.RefreshSourceRefreshed:
			usable++
		case recall.RefreshSourceDegraded:
			usable++
			degraded = true
		case recall.RefreshSourceFailed:
			degraded = true
		case recall.RefreshSourceSkipped:
			if targeted {
				degraded = true
			}
		}
	}
	if attempted == 0 || usable == 0 {
		return recall.RefreshFailed
	}
	if degraded {
		return recall.RefreshDegraded
	}
	return recall.RefreshSucceeded
}
