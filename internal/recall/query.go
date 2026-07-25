package recall

import "time"

// QueryRequest is the host-facing contract. Everything Recall knows about the
// caller arrives here: it reads no transcript, no session log, and holds no
// per-conversation state between requests.
type QueryRequest struct {
	// Query is the exact current request text.
	Query string `json:"query"`

	Profile string `json:"profile,omitempty"`
	Scope   *Scope `json:"scope,omitempty"`

	// AsOf bounds the request to a past instant. Sources that cannot honor it
	// are excluded and reported; none answers from current state.
	AsOf *time.Time `json:"as_of,omitempty"`

	// Context is bounded prior message text. It reaches only adapters that
	// declare context expansion. There are no per-message weights: a weight
	// with no code path consuming it would be a dead field.
	Context []string `json:"context,omitempty"`

	// ConversationID correlates requests for the host's own bookkeeping. Recall
	// stores nothing under it.
	ConversationID string `json:"conversation_id,omitempty"`

	// RequestID is a correlation identity for tracing and logs. It is not an
	// idempotency key: Recall stores no request results.
	RequestID string `json:"request_id,omitempty"`

	// SuppressLineages are roots the host has already shown. Suppression state
	// lives with the host, which owns the conversation; Recall applies the list
	// in pre-reply mode and ignores it for explicit requests.
	SuppressLineages []LineageRoot `json:"suppress_lineages,omitempty"`

	Mode   InvocationMode `json:"mode"`
	Budget Budget         `json:"budget"`
	Limit  int            `json:"limit"`
}

// Scope narrows which sources and records a request may reach.
type Scope struct {
	SourceIDs   []string     `json:"source_ids,omitempty"`
	RecordTypes []RecordType `json:"record_types,omitempty"`
	Since       *time.Time   `json:"since,omitempty"`
	Until       *time.Time   `json:"until,omitempty"`
}

// Budget bounds one request in time and output size.
type Budget struct {
	LatencyMS      int `json:"latency_ms"`
	ResponseTokens int `json:"response_tokens"`
}

// QueryResponse is what a host receives. Outcome and Coverage are independent:
// a request may abstain with complete coverage, or answer with degraded
// coverage, and both facts matter.
type QueryResponse struct {
	Results []Result `json:"results"`

	// SourceOutcomes reports every eligible source, including those that were
	// skipped, denied, or failed. A source that did not answer is visible here
	// rather than silently absent.
	SourceOutcomes []SourceReport `json:"source_outcomes"`

	Plan       Plan          `json:"plan"`
	Suppressed []Suppression `json:"suppressed,omitempty"`

	Outcome  Outcome  `json:"outcome"`
	Coverage Coverage `json:"coverage"`

	// Truncated means budget shaping dropped trailing results. Truncation is
	// not degradation: coverage describes which sources were searched, not how
	// much of the answer fit.
	Truncated      bool `json:"truncated"`
	DroppedResults int  `json:"dropped_results,omitempty"`

	Elapsed time.Duration `json:"elapsed_ns"`
}

// Result is one cluster in final order: an entity or fact, the evidence
// supporting it, and why it ranked where it did.
type Result struct {
	// Primary is the candidate chosen to represent the cluster.
	Primary Candidate `json:"primary"`

	// Members are the cluster's lineage groups. Two members mean two
	// independent records; two candidates inside one member mean one record
	// seen twice.
	Members []ClusterMember `json:"members,omitempty"`

	Explanation Explanation `json:"explanation"`
	Score       float64     `json:"score"`
}

// ClusterMember is one lineage group: every candidate projecting a single
// original record.
type ClusterMember struct {
	LineageRoot LineageRoot `json:"lineage_root"`
	Candidates  []Candidate `json:"candidates"`
}

// SourceReport is one source's contribution to a response.
type SourceReport struct {
	SourceUID SourceUID     `json:"source_uid"`
	SourceID  string        `json:"source_id"`
	Outcome   SearchOutcome `json:"outcome"`

	// Reason explains a non-success outcome in terms a person can act on:
	// as_of_unsupported, budget_exhausted, denied, unreachable.
	Reason string `json:"reason,omitempty"`

	Candidates int           `json:"candidates"`
	Elapsed    time.Duration `json:"elapsed_ns"`
	ColdStart  time.Duration `json:"cold_start_ns,omitempty"`

	SourceWatermark string     `json:"source_watermark,omitempty"`
	IndexGeneration string     `json:"index_generation,omitempty"`
	ConfirmedAt     *time.Time `json:"confirmed_at,omitempty"`

	Diagnostics map[string]any `json:"diagnostics,omitempty"`
}

// Plan is the resolved retrieval plan that produced a response. It is returned
// so a caller can see what was actually searched rather than inferring it.
type Plan struct {
	Profile   string        `json:"profile"`
	Sources   []PlanSource  `json:"sources"`
	Deadline  time.Time     `json:"deadline"`
	Reserve   time.Duration `json:"fusion_reserve_ns"`
	Limit     int           `json:"limit"`
	RankConst float64       `json:"rank_constant"`
	Corrobor  float64       `json:"corroboration_cap"`
}

// PlanSource records one eligibility decision and the budget it received.
type PlanSource struct {
	SourceUID SourceUID     `json:"source_uid"`
	SourceID  string        `json:"source_id"`
	Eligible  bool          `json:"eligible"`
	Reason    string        `json:"reason,omitempty"`
	Limit     int           `json:"limit"`
	Timeout   time.Duration `json:"timeout_ns"`
	Prior     float64       `json:"prior"`
}

// Suppression records a candidate withheld from display, and why. Counts are
// reported so a host can say what it is not showing.
type Suppression struct {
	Reason      string      `json:"reason"`
	Count       int         `json:"count"`
	LineageRoot LineageRoot `json:"lineage_root,omitempty"`
}

// Suppression reasons.
const (
	SuppressLineageSeen = "lineage_suppressed"
	SuppressSensitivity = "sensitivity"
	SuppressDuplicate   = "near_duplicate"
	SuppressDiversity   = "diversity"
)
