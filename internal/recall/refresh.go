package recall

import "time"

// RefreshRequest asks Recall to update one checkpoint-capable source, or every
// eligible checkpoint-capable source in the active profile when SourceID is
// empty.
type RefreshRequest struct {
	Profile  string `json:"profile,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Full     bool   `json:"full,omitempty"`
}

// RefreshOutcome states whether the requested maintenance produced usable
// fresh state. It deliberately mirrors the three semantic severities used by
// query across CLI, HTTP, and MCP.
type RefreshOutcome string

const (
	RefreshSucceeded RefreshOutcome = "refreshed"
	RefreshDegraded  RefreshOutcome = "degraded"
	RefreshFailed    RefreshOutcome = "failed"
)

// RefreshSourceStatus is one source's part of a refresh.
type RefreshSourceStatus string

const (
	RefreshSourceRefreshed RefreshSourceStatus = "refreshed"
	RefreshSourceDegraded  RefreshSourceStatus = "degraded"
	RefreshSourceFailed    RefreshSourceStatus = "failed"
	RefreshSourceSkipped   RefreshSourceStatus = "skipped"
)

// RefreshReason is the closed, machine-actionable reason a source did not
// refresh. Adapter error prose is intentionally not exposed through a host
// surface.
type RefreshReason string

const (
	RefreshDisabled              RefreshReason = "disabled"
	RefreshDenied                RefreshReason = "sensitivity_denied"
	RefreshCheckpointUnsupported RefreshReason = "checkpoint_unsupported"
	RefreshSourceNotConfigured   RefreshReason = "source_not_configured"
	RefreshSourceNotInProfile    RefreshReason = "source_not_in_profile"
	RefreshInitializeFailed      RefreshReason = "initialize_failed"
	RefreshOperationFailed       RefreshReason = "refresh_failed"
	RefreshCancelled             RefreshReason = "cancelled"
	RefreshTimedOut              RefreshReason = "timeout"
	RefreshUnhealthy             RefreshReason = "unhealthy"
)

// RefreshSourceOutcome reports every source considered by the request. Health
// is the post-refresh health whenever the adapter produced one.
type RefreshSourceOutcome struct {
	SourceID  string              `json:"source_id"`
	SourceUID SourceUID           `json:"source_uid,omitempty"`
	Status    RefreshSourceStatus `json:"status"`
	Reason    RefreshReason       `json:"reason,omitempty"`
	Health    *Health             `json:"health,omitempty"`
	Elapsed   time.Duration       `json:"elapsed_ns,omitempty"`
}

// RefreshResponse is the transport-neutral refresh result.
type RefreshResponse struct {
	Outcome RefreshOutcome         `json:"outcome"`
	Sources []RefreshSourceOutcome `json:"sources"`
	Elapsed time.Duration          `json:"elapsed_ns"`
}
