package recall

import "time"

// RefreshRequest asks Recall to update one source, or every eligible
// checkpoint-capable source in the active profile when SourceID is empty. A
// named source that cannot checkpoint is health-probed instead of skipped.
type RefreshRequest struct {
	// Profile selects the configured source set.
	Profile string `json:"profile,omitempty"`
	// SourceID selects one configured source; empty selects all eligible sources.
	SourceID string `json:"source_id,omitempty"`
	// Full requests a complete rebuild rather than an incremental refresh.
	Full bool `json:"full,omitempty"`
}

// RefreshOutcome states whether the requested maintenance produced usable
// fresh state. It deliberately mirrors the three semantic severities used by
// query across CLI, HTTP, and MCP.
type RefreshOutcome string

const (
	// RefreshSucceeded means every selected source refreshed successfully.
	RefreshSucceeded RefreshOutcome = "refreshed"
	// RefreshDegraded means some selected sources did not refresh successfully.
	RefreshDegraded RefreshOutcome = "degraded"
	// RefreshFailed means no selected source produced usable refreshed state.
	RefreshFailed RefreshOutcome = "failed"
)

// RefreshSourceStatus is one source's part of a refresh.
type RefreshSourceStatus string

const (
	// RefreshSourceRefreshed means the source published fresh state.
	RefreshSourceRefreshed RefreshSourceStatus = "refreshed"
	// RefreshSourceDegraded means the source retained usable but degraded state.
	RefreshSourceDegraded RefreshSourceStatus = "degraded"
	// RefreshSourceFailed means the source produced no usable refresh result.
	RefreshSourceFailed RefreshSourceStatus = "failed"
	// RefreshSourceSkipped means the source was not refreshable for this request.
	RefreshSourceSkipped RefreshSourceStatus = "skipped"
)

// RefreshReason is the closed, machine-actionable reason a source did not
// refresh. Safe adapter diagnostic detail is carried separately.
type RefreshReason string

const (
	// RefreshDisabled means the source is disabled.
	RefreshDisabled RefreshReason = "disabled"
	// RefreshDenied means policy excludes the source.
	RefreshDenied RefreshReason = "sensitivity_denied"
	// RefreshCheckpointUnsupported means the adapter cannot refresh checkpoints.
	RefreshCheckpointUnsupported RefreshReason = "checkpoint_unsupported"
	// RefreshSourceNotConfigured means no configured source has the requested ID.
	RefreshSourceNotConfigured RefreshReason = "source_not_configured"
	// RefreshSourceNotInProfile means the active profile excludes the source.
	RefreshSourceNotInProfile RefreshReason = "source_not_in_profile"
	// RefreshInitializeFailed means the adapter could not initialize.
	RefreshInitializeFailed RefreshReason = "initialize_failed"
	// RefreshOperationFailed means the adapter's refresh operation failed.
	RefreshOperationFailed RefreshReason = "refresh_failed"
	// RefreshCancelled means the caller cancelled the refresh.
	RefreshCancelled RefreshReason = "cancelled"
	// RefreshTimedOut means the refresh exceeded its deadline.
	RefreshTimedOut RefreshReason = "timeout"
	// RefreshUnhealthy means refreshed state was not usable.
	RefreshUnhealthy RefreshReason = "unhealthy"
)

// RefreshSourceOutcome reports every source considered by the request. Health
// is the post-refresh health whenever the adapter produced one.
type RefreshSourceOutcome struct {
	// SourceID is the configured source name.
	SourceID string `json:"source_id"`
	// SourceUID identifies the configured source instance.
	SourceUID SourceUID `json:"source_uid,omitempty"`
	// Status is this source's refresh result.
	Status RefreshSourceStatus `json:"status"`
	// Reason explains a non-refreshed status.
	Reason RefreshReason `json:"reason,omitempty"`
	// Health is the post-refresh health when the adapter produced one.
	Health *Health `json:"health,omitempty"`
	// DiagnosticDetail is the adapter's safe, actionable health detail when it
	// supplied one.
	DiagnosticDetail string `json:"diagnostic_detail,omitempty"`
	// Elapsed is this source's refresh duration.
	Elapsed time.Duration `json:"elapsed_ns,omitempty"`
}

// RefreshResponse is the transport-neutral refresh result.
type RefreshResponse struct {
	// Outcome is the aggregate refresh result.
	Outcome RefreshOutcome `json:"outcome"`
	// Sources reports every source considered by the request.
	Sources []RefreshSourceOutcome `json:"sources"`
	// Elapsed is the end-to-end refresh duration.
	Elapsed time.Duration `json:"elapsed_ns"`
}
