package app

import (
	"context"
	"errors"
	"sync"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// Refresh updates one source, or every eligible checkpoint-capable source in
// the selected profile. A named source that cannot checkpoint is health-probed
// instead of skipped. Sources are reported in profile order even though
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
			wg.Add(1)
			go func(i int, inst *config.SourceInstance) {
				defer wg.Done()
				results[i] = a.initializeAndRefreshOne(ctx, inst, req.Full, req.SourceID != "")
			}(i, inst)
		}
	}
	wg.Wait()

	resp.Sources = results
	resp.Outcome = aggregateRefresh(results)
	resp.Elapsed = a.now().Sub(started)
	return resp, nil
}

func (a *App) initializeAndRefreshOne(
	parent context.Context,
	inst *config.SourceInstance,
	full bool,
	named bool,
) recall.RefreshSourceOutcome {
	out := recall.RefreshSourceOutcome{SourceID: inst.ID, SourceUID: inst.UID}
	started := a.now()

	ctx, cancel := context.WithTimeout(parent, inst.Timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	manifest, err := a.registry.Initialize(ctx, inst)
	if err != nil {
		out.Status = recall.RefreshSourceFailed
		out.Reason = classifyRefreshError(err, recall.RefreshInitializeFailed)
		out.Elapsed = a.now().Sub(started)
		return out
	}
	checkpoint := manifest.Can(recall.CapCheckpoint)
	if !checkpoint && !named {
		out.Status = recall.RefreshSourceSkipped
		out.Reason = recall.RefreshCheckpointUnsupported
		out.Elapsed = a.now().Sub(started)
		return out
	}

	adp, err := a.registry.Adapter(inst)
	if err != nil {
		out.Status = recall.RefreshSourceFailed
		out.Reason = classifyRefreshError(err, refreshFailureReason(checkpoint))
		out.Elapsed = a.now().Sub(started)
		return out
	}
	var health recall.Health
	if checkpoint {
		health, err = adp.Refresh(ctx, protocol.RefreshParams{Deadline: deadline, Full: full})
	} else {
		health, err = adp.Health(ctx)
	}
	out.Elapsed = a.now().Sub(started)
	attachRefreshHealth(&out, health)
	if err != nil {
		out.Status = recall.RefreshSourceFailed
		out.Reason = classifyRefreshError(err, refreshFailureReason(checkpoint))
		return out
	}
	applyRefreshHealth(&out, health, checkpoint)
	return out
}

func refreshFailureReason(checkpoint bool) recall.RefreshReason {
	if checkpoint {
		return recall.RefreshOperationFailed
	}
	return recall.RefreshUnhealthy
}

func attachRefreshHealth(out *recall.RefreshSourceOutcome, health recall.Health) {
	if health.Status == "" {
		return
	}
	out.Health = &health
	out.DiagnosticDetail, _ = health.Diagnostics["detail"].(string)
	if out.DiagnosticDetail == "" {
		out.DiagnosticDetail, _ = health.Diagnostics["last_refresh_error"].(string)
	}
}

func applyRefreshHealth(out *recall.RefreshSourceOutcome, health recall.Health, honorCheckpointProgress bool) {
	switch health.Status {
	case recall.HealthHealthy:
		out.Status = recall.RefreshSourceRefreshed
	case recall.HealthDegraded:
		if honorCheckpointProgress && health.CheckpointProgress == recall.CheckpointAdvanced {
			// The maintenance operation moved comparable checkpoint counters
			// forward without regression. Keep the attached partial health
			// honest for queries, but do not call successful progress a failed
			// scheduler pass. Health probes do not set this.
			out.Status = recall.RefreshSourceRefreshed
		} else {
			out.Status = recall.RefreshSourceDegraded
			out.Reason = recall.RefreshUnhealthy
		}
	default:
		out.Status = recall.RefreshSourceFailed
		out.Reason = recall.RefreshUnhealthy
	}
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

func aggregateRefresh(results []recall.RefreshSourceOutcome) recall.RefreshOutcome {
	usable, degraded := 0, false
	for _, result := range results {
		switch result.Status {
		case recall.RefreshSourceRefreshed:
			usable++
		case recall.RefreshSourceDegraded:
			usable++
			degraded = true
		case recall.RefreshSourceFailed:
			degraded = true
		case recall.RefreshSourceSkipped:
			// Skips are the declared non-target set for all-source refresh:
			// disabled, denied, or lacking checkpoint capability. They do not
			// degrade a usable refresh, but an all-skipped request still fails
			// below because it produced no usable state.
		}
	}
	if usable == 0 {
		return recall.RefreshFailed
	}
	if degraded {
		return recall.RefreshDegraded
	}
	return recall.RefreshSucceeded
}
