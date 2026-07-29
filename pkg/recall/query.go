package recall

import (
	"errors"
	"time"
)

// QueryRequest is the host-facing contract. Everything Recall knows about the
// caller arrives here: it reads no transcript, no session log, and holds no
// per-conversation state between requests.
type QueryRequest struct {
	// Query is the exact current request text.
	Query string `json:"query"`

	// Profile selects the configured source set.
	Profile string `json:"profile,omitempty"`
	// Scope narrows sources and records for this request.
	Scope *Scope `json:"scope,omitempty"`

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

	// Mode identifies whether this is explicit or pre-reply recall.
	Mode InvocationMode `json:"mode"`
	// Budget bounds latency and rendered output.
	Budget Budget `json:"budget"`
	// Limit bounds the number of results.
	Limit int `json:"limit"`
}

// ErrUnsatisfiableScope means the request named a scope this profile cannot
// satisfy as written — every source it named is outside the profile, or is not
// configured at all.
//
// It is an error rather than an empty answer on purpose. Everywhere else,
// `coverage: complete` means every eligible source was asked; a scope that
// narrowed the source set to nothing makes that claim over nothing, and a
// caller reading outcome and coverage concludes the named source holds nothing
// on the subject. That is the one thing this system may not say by accident,
// so the request is refused and names the profile that would serve it.
var ErrUnsatisfiableScope = errors.New("unsatisfiable scope")

// Scope narrows which sources and records a request may reach.
type Scope struct {
	// SourceIDs restricts the request to configured source names.
	SourceIDs []string `json:"source_ids,omitempty"`
	// RecordTypes restricts the request to record kinds.
	RecordTypes []RecordType `json:"record_types,omitempty"`
	// Since is the inclusive lower time bound.
	Since *time.Time `json:"since,omitempty"`
	// Until is the inclusive upper time bound.
	Until *time.Time `json:"until,omitempty"`

	// Entities and Project are source-local structured constraints. A document
	// corpus can match lexical entity tokens, while a task source can match
	// labels, contexts, or project handles; the core cannot give those
	// different meanings one global implementation.
	//
	// Both reach every eligible source. An adapter either enforces each one or
	// skips with filter_unsupported before retrieval; a source that understands
	// the constraint but is not the named project may skip as not_applicable.
	Entities []string `json:"entities,omitempty"`
	// Project is a source-local project constraint.
	Project string `json:"project,omitempty"`
}

// Budget bounds one request in time and output size.
type Budget struct {
	// LatencyMS is the end-to-end latency ceiling in milliseconds.
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
	// SurfaceStructured is the response serialized whole: `recall query --json
	// --explain`, the HTTP body, the MCP tool's structured content. It is the
	// default because it is what a caller receives when no surface projects it,
	// and it is the most expensive of them.
	SurfaceStructured ResponseSurface = "structured"

	// SurfaceStructuredPointer is the pointer tier serialized: `recall query
	// --json`. It is to SurfaceStructured what SurfacePointer is to
	// SurfaceExplained — the same response with the diagnostic tier projected
	// out — and it exists as its own surface because a budget denominated in
	// the whole serialization would shape a projection of it as though the
	// dropped fields were still being paid for, and answer a machine caller
	// with fewer results than the same query answers a human.
	SurfaceStructuredPointer ResponseSurface = "structured_pointer"

	// SurfaceTool is an MCP tool result: the pointer projection as structured
	// content and the text rendering of it, inside the JSON-RPC envelope that
	// carries both.
	SurfaceTool ResponseSurface = "tool"

	// SurfaceToolExplained is an MCP tool result whose structured half is the
	// complete serialization, which is what the tool's `explain` argument asks
	// for. It is a separate surface for the same reason SurfaceStructuredPointer
	// is: the two differ by roughly the whole per-source ledger and plan, and
	// pricing the projected one as the complete one would answer a model with
	// fewer results than it asked for.
	SurfaceToolExplained ResponseSurface = "tool_explained"

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
	// Results are fused and ranked evidence pointers.
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

	// Plan is the resolved source and fusion plan.
	Plan Plan `json:"plan"`
	// Suppressed records evidence withheld by policy or prior display.
	Suppressed []Suppression `json:"suppressed,omitempty"`

	// Omitted names what the response budget removed from this response,
	// outside the results, in a closed vocabulary. Naming it is the difference
	// between a fact that did not fit and a fact that does not exist.
	Omitted []Omission `json:"omitted,omitempty"`

	// Outcome states whether Recall answered, abstained, or failed.
	Outcome Outcome `json:"outcome"`
	// Coverage states whether every eligible source was searched.
	Coverage Coverage `json:"coverage"`

	// Truncated means budget shaping dropped trailing results. Truncation is
	// not degradation: coverage describes which sources were searched, not how
	// much of the answer fit.
	Truncated bool `json:"truncated"`
	// DroppedResults is how many trailing results budget shaping removed.
	DroppedResults int `json:"dropped_results,omitempty"`

	// Elapsed is the end-to-end request duration.
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
	// Primary is the candidate chosen to represent the cluster. For chunks of
	// one document this may be a matched content chunk rather than the
	// preview-only chunk that earned the record's score; Explanation names the
	// score-producing evidence in that case.
	Primary Candidate `json:"primary"`

	// Members are the cluster's lineage groups. Two candidates inside one
	// member mean one record seen twice; two members mean two lineage roots,
	// which is two records unless one of them is reported as a duplicate_view
	// suppression. What is independent evidence is the corroboration count in
	// the explanation, never the member count.
	Members []ClusterMember `json:"members,omitempty"`

	// Explanation records every input that affected ranking.
	Explanation Explanation `json:"explanation"`
	// Score is the final fused score.
	Score float64 `json:"score"`
}

// ClusterMember is one lineage group: every candidate projecting a single
// original record.
type ClusterMember struct {
	// LineageRoot identifies this member's original record.
	LineageRoot LineageRoot `json:"lineage_root"`
	// Candidates are the source views grouped under this root.
	Candidates []Candidate `json:"candidates"`
}

// SourceReport is one source's contribution to a response.
type SourceReport struct {
	// SourceUID identifies the configured source instance.
	SourceUID SourceUID `json:"source_uid"`
	// SourceID is the configured source name.
	SourceID string `json:"source_id"`
	// Outcome states how this source's search completed.
	Outcome SearchOutcome `json:"outcome"`

	// Reason explains a non-success outcome in terms a person can act on:
	// as_of_unsupported, budget_exhausted, denied, unreachable.
	Reason string `json:"reason,omitempty"`

	// Candidates is the number of candidates returned by the source.
	Candidates int `json:"candidates"`
	// Elapsed is the source search duration.
	Elapsed time.Duration `json:"elapsed_ns"`
	// ColdStart is adapter initialization time charged to this request.
	ColdStart time.Duration `json:"cold_start_ns,omitempty"`

	// SourceWatermark identifies the source state searched.
	SourceWatermark string `json:"source_watermark,omitempty"`
	// IndexGeneration identifies the index generation searched.
	IndexGeneration string `json:"index_generation,omitempty"`
	// ConfirmedAt is the source's most recent complete confirmation.
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`

	// Diagnostics carries safe source-specific detail.
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
}

// Plan is the resolved retrieval plan that produced a response. It is returned
// so a caller can see what was actually searched rather than inferring it.
type Plan struct {
	// Profile is the resolved profile name.
	Profile string `json:"profile"`
	// Sources records every configured source considered.
	Sources []PlanSource `json:"sources"`
	// Deadline is the absolute request deadline.
	Deadline time.Time `json:"deadline"`
	// Reserve is time held back for fusion and rendering.
	Reserve time.Duration `json:"fusion_reserve_ns"`

	// Limit is the result budget that was in force, from the request when it
	// named one and from configuration otherwise. Zero means unbounded.
	Limit int `json:"limit"`
	// RankConst is the reciprocal-rank fusion constant.
	RankConst float64 `json:"rank_constant"`
	// Corrobor is the corroboration contribution cap.
	Corrobor float64 `json:"corroboration_cap"`

	// RelevanceFloor is the least relevance that reached the fused pool. It is
	// here for the same reason the two above are: these are the values that
	// decide what a caller was shown, and a rule that shortens an answer without
	// appearing in the plan is a rule nobody can check.
	RelevanceFloor float64 `json:"relevance_floor"`
}

// FusionRules is the fusion configuration a response reports back: what decided
// the ordering, and what decided the length.
//
// It exists so the plan is rendered from one value rather than from a growing
// list of positional floats, and so adding a rule that shapes an answer means
// adding a field here — which is the same as saying it will be reported.
type FusionRules struct {
	// RankConstant is the reciprocal-rank fusion constant.
	RankConstant float64
	// CorroborationCap bounds independent-evidence contribution.
	CorroborationCap float64
	// RelevanceFloor excludes candidates below this shared-scale relevance.
	RelevanceFloor float64

	// Limit is the budget in force for the request being answered, not the
	// configured default, because a caller who overrode it should read back what
	// applied to them.
	Limit int
}

// PlanSource records one eligibility decision and the budget it received.
type PlanSource struct {
	// SourceUID identifies the configured source instance.
	SourceUID SourceUID `json:"source_uid"`
	// SourceID is the configured source name.
	SourceID string `json:"source_id"`
	// Eligible reports whether the source may answer this request.
	Eligible bool `json:"eligible"`
	// Reason explains ineligibility.
	Reason string `json:"reason,omitempty"`
	// Diagnostics carries safe planning detail.
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
	// Limit is this source's candidate limit.
	Limit int `json:"limit"`
	// Timeout is this source's allotted duration.
	Timeout time.Duration `json:"timeout_ns"`
	// Prior is this source's configured ranking prior.
	Prior float64 `json:"prior"`
}

// Suppression records a candidate withheld from display, and why. Counts are
// reported so a host can say what it is not showing.
type Suppression struct {
	// Reason is the stable suppression reason.
	Reason string `json:"reason"`
	// Count is the number of results suppressed for this reason.
	Count int `json:"count"`
	// LineageRoot names the suppressed lineage when suppression is per record.
	LineageRoot LineageRoot `json:"lineage_root,omitempty"`

	// FusedInto names the result this one was folded into, when the record is
	// still in the answer under another view of it. It is what lets a consumer
	// tell "withheld and gone" from "withheld because you already have it": a
	// count alone cannot, and a reader that cannot tell has to assume the
	// worse of the two. Set for SuppressDuplicateView and empty for every
	// reason that withheld a record outright.
	FusedInto LineageRoot `json:"fused_into,omitempty"`
}

// Suppression reasons.
const (
	SuppressLineageSeen = "lineage_suppressed"
	SuppressSensitivity = "sensitivity"
	SuppressDuplicate   = "near_duplicate"
	SuppressDiversity   = "diversity"

	// SuppressRelevanceFloor names a result nothing in which the sources
	// reported as being about the query, withheld by the profile's configured
	// relevance floor. Like the two above it, it is a display decision taken
	// after fusion over a whole cluster, and it names that cluster's lineage
	// root.
	//
	// It is reported for the same reason as every other withheld thing: an
	// answer that silently dropped what a source returned would be
	// indistinguishable from a corpus that never held it, and this rule is the
	// one most likely to be blamed when a record someone expected is missing.
	//
	// It never accounts for an empty answer. A floor may withhold but not
	// abstain, so when nothing would otherwise be shown it stands down and this
	// reason does not appear.
	SuppressRelevanceFloor = "below_relevance_floor"

	// SuppressDuplicateView names a second view of a record that is already in
	// the answer: the same record identifier, content fingerprint, and source
	// revision reached through a second source instance. It is the one reason
	// whose record is still in the response, named by FusedInto — the view
	// itself is a member of the cluster that absorbed it, until a response
	// budget compresses that cluster to a pointer. What was withheld is the
	// result slot, and saying so is what keeps a fused answer from reading as
	// a corpus that held one copy.
	SuppressDuplicateView = "duplicate_view"
)
