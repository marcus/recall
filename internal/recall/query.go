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

	// Entities and Project are source-local structured constraints. A document
	// corpus can match lexical entity tokens, while a task source can match
	// labels, contexts, or project handles; the core cannot give those
	// different meanings one global implementation.
	//
	// Both reach every eligible source. An adapter either enforces each one or
	// skips with filter_unsupported before retrieval; a source that understands
	// the constraint but is not the named project may skip as not_applicable.
	Entities []string `json:"entities,omitempty"`
	Project  string   `json:"project,omitempty"`
}

// Budget bounds one request in time and output size.
type Budget struct {
	LatencyMS int `json:"latency_ms"`

	// ResponseTokens bounds the whole rendered response, not its excerpts: the
	// frame a surface prints whatever it finds is charged first, then results
	// until the rest is spent. Zero or less is unbounded here; a product
	// surface substitutes [DefaultResponseTokens] for an unset budget, because
	// a caller with a context window must not be handed an unbounded response
	// by default while a library caller keeps what it asked for.
	ResponseTokens int `json:"response_tokens"`

	// Surface names the rendering the budget is denominated in. The same
	// results cost an order of magnitude more serialized than as pointers, so
	// a budget means nothing until the response is priced as what the caller
	// will actually receive. An unrecognized surface is priced as
	// [SurfaceStructured], which never underestimates a projection of it.
	//
	// It is the caller's declaration of what it will consume, and a transport
	// supplies only the default: unset, a request is priced as the form that
	// transport sends. A client that renders a projection of what it receives —
	// `recall query --server` prints pointers from a JSON body — declares that
	// projection and is priced for it, so a query answers identically in
	// process and over a socket. A caller that declares a projection it does
	// not apply misprices only itself.
	Surface ResponseSurface `json:"surface,omitempty"`
}

// ResponseSurface names one rendering of a response, for pricing it.
//
// It travels with the request so a response shaped by `recall serve` is shaped
// for the surface that will print it, exactly as an in-process one is.
type ResponseSurface string

const (
	// SurfaceStructured is the response serialized whole: `--json`, the HTTP
	// body, the MCP tool's structured content. It is the default because it is
	// what a caller receives when no surface projects it, and it is the most
	// expensive of them.
	SurfaceStructured ResponseSurface = "structured"

	// SurfaceTool is an MCP tool result: the structured response and the text
	// projection of it, inside the JSON-RPC envelope that carries both.
	SurfaceTool ResponseSurface = "tool"

	// SurfacePointer is the default human tier: rank, locator, title, excerpt.
	SurfacePointer ResponseSurface = "pointer"

	// SurfaceExplained is the human diagnostic tier: the pointer tier plus
	// per-result provenance, lineage, score explanations, the source ledger,
	// and the resolved plan.
	SurfaceExplained ResponseSurface = "explained"
)

// DefaultResponseTokens is the ceiling a product surface applies when the
// caller named no budget.
//
// It is generous on purpose: about 32 KB of human output, a few percent of a
// large context window, well above what any ordinary query renders and well
// below the 203 KB one unbounded query once produced. The point is not to tune
// a response, it is that no query can spend an unbounded amount of a caller's
// context without being asked to. A caller that wants everything says
// --budget-tokens -1.
const DefaultResponseTokens = 8000

// QueryResponse is what a host receives. Outcome and Coverage are independent:
// a request may abstain with complete coverage, or answer with degraded
// coverage, and both facts matter.
type QueryResponse struct {
	Results []Result `json:"results"`

	// SourceOutcomes reports every eligible source, including those that were
	// skipped, denied, or failed. A source that did not answer is visible here
	// rather than silently absent.
	SourceOutcomes []SourceReport `json:"source_outcomes"`

	// SourceSummary stands in for SourceOutcomes when the response budget could
	// not afford the per-source ledger. It is present only then, and Omitted
	// names what it replaced: an absent ledger is a budget fact, never evidence
	// that no source was asked.
	SourceSummary *SourceSummary `json:"source_summary,omitempty"`

	Plan       Plan          `json:"plan"`
	Suppressed []Suppression `json:"suppressed,omitempty"`

	// Omitted names what the response budget removed from this response,
	// outside the results, in a closed vocabulary. Naming it is the difference
	// between a fact that did not fit and a fact that does not exist.
	Omitted []Omission `json:"omitted,omitempty"`

	Outcome  Outcome  `json:"outcome"`
	Coverage Coverage `json:"coverage"`

	// Truncated means budget shaping dropped trailing results. Truncation is
	// not degradation: coverage describes which sources were searched, not how
	// much of the answer fit.
	Truncated      bool `json:"truncated"`
	DroppedResults int  `json:"dropped_results,omitempty"`

	Elapsed time.Duration `json:"elapsed_ns"`
}

// Omission names one thing a response budget removed from a response's frame.
type Omission string

const (
	// OmittedSourceOutcomes means the per-source ledger was replaced by
	// [QueryResponse.SourceSummary].
	OmittedSourceOutcomes Omission = "source_outcomes"

	// OmittedPlanSources means the plan's per-source list was dropped. The
	// plan's header stays: it describes the request in a line, while the list
	// is the part that grows with the profile.
	OmittedPlanSources Omission = "plan_sources"
)

// SourceSummary is the per-source ledger reduced to what the ledger is read
// for: how many sources reported each outcome, and which ones could not answer.
//
// The per-source freshness evidence is what it drops, because that is the part
// that grows with the profile and `recall sources` answers it on demand. What
// it must never drop is Degraded — a source that could not answer is named
// wherever the ledger would have named it, or the response would be claiming
// complete evidence it does not have.
type SourceSummary struct {
	// Sources is how many the request touched, answered or not.
	Sources int `json:"sources"`

	// Outcomes counts sources by their reported outcome.
	Outcomes map[SearchOutcome]int `json:"outcomes"`

	// Degraded names every source that was eligible and could not answer, as
	// "source_id (reason)".
	Degraded []string `json:"degraded,omitempty"`
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
	SourceUID   SourceUID      `json:"source_uid"`
	SourceID    string         `json:"source_id"`
	Eligible    bool           `json:"eligible"`
	Reason      string         `json:"reason,omitempty"`
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
	Limit       int            `json:"limit"`
	Timeout     time.Duration  `json:"timeout_ns"`
	Prior       float64        `json:"prior"`
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
