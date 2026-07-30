package recall

import "time"

// Candidate is a compact search result from one source, ready for fusion.
//
// It is a pointer, not a payload: Title and Excerpt are bounded previews and
// Locator is how the caller gets more. Adapters fill everything except
// SourceUID and SourceID, which the core attaches so an adapter cannot claim
// an identity configuration did not give it.
type Candidate struct {
	// CandidateID is stable within a source revision.
	CandidateID string `json:"candidate_id"`

	// SourceUID is the immutable configured source identity attached by the core.
	SourceUID SourceUID `json:"source_uid"`
	// SourceID is the configured source name attached by the core.
	SourceID string `json:"source_id"`

	// SourceRecordID is the native or adapter-defined record identity.
	SourceRecordID string `json:"source_record_id"`

	// Locator is the reference used to expand this candidate.
	Locator Locator `json:"locator"`

	// DerivedFrom names upstream records this candidate projects. It is the
	// primary lineage edge: a signal projecting a task declares that task's
	// locator, and the two never corroborate each other.
	DerivedFrom []Locator `json:"derived_from,omitempty"`

	// RecordType classifies the source record.
	RecordType RecordType `json:"record_type"`
	// Title is a compact human-readable record label.
	Title string `json:"title"`
	// Excerpt is a bounded evidence preview.
	Excerpt string `json:"excerpt,omitempty"`

	// ExcerptKind reports whether the excerpt is the span that matched or the
	// record's opening. Empty is a third state and not a default: the source
	// asserts nothing, either because it does not select excerpts by query or
	// because it could not establish which this one is.
	ExcerptKind ExcerptKind `json:"excerpt_kind,omitempty"`

	// LocalRank is the candidate's position in its source's own result list,
	// one-based. It is the only MANDATORY relevance signal; fusion consumes it
	// and the optional Relevance below, and nothing else.
	LocalRank int `json:"local_rank"`

	// LocalScore is the source's native score. It is diagnostic: scales differ
	// between engines, so it is never compared across sources.
	LocalScore *float64 `json:"local_score,omitempty"`

	// Relevance is the source's estimate, in [0,1], of how much this record is
	// ABOUT the query. Unlike LocalScore it IS comparable across sources,
	// because what is fixed is the definition rather than the scale: every
	// source computes it through [Relevance], which is where the formula and
	// the reasoning behind both of its factors live.
	//
	// Nil means the source asserts nothing, and fusion reads it as 1.0 — the
	// same ordering it produced before this field existed. That is what keeps
	// an out-of-tree adapter working across this change, and it is also a
	// standing advantage over sources that report honestly, so a source that
	// can compute this should.
	Relevance *float64 `json:"relevance,omitempty"`

	// MatchSignals name why the adapter returned this candidate.
	MatchSignals []MatchSignal `json:"match_signals,omitempty"`

	// The four timestamps below answer different questions and are never
	// collapsed. ObservedAt is when Recall read the record; ConfirmedAt is when
	// a complete successful source boundary confirmed it; EventTime is when the
	// underlying event happened; ValidFrom/ValidTo bound when a fact was true.
	// ObservedAt is when Recall read the record.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	// ConfirmedAt is when a complete source boundary confirmed the record.
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// EventTime is when the underlying event happened.
	EventTime *time.Time `json:"event_time,omitempty"`
	// ValidFrom is when a fact began to be true.
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	// ValidTo is when a fact stopped being true.
	ValidTo *time.Time `json:"valid_to,omitempty"`

	// SourceRevision identifies the source version this candidate came from.
	SourceRevision string `json:"source_revision,omitempty"`

	// Sensitivity may raise the source's configured floor, never lower it.
	Sensitivity Sensitivity `json:"sensitivity"`

	// Metadata carries small typed fields useful for routing and display.
	// Structured sources keep their fields here rather than flattening a task
	// or person into anonymous text.
	Metadata map[string]any `json:"metadata,omitempty"`

	// ContentFingerprint is an advisory normalized content hash. It may collapse
	// candidates for corroboration counting when no derivation edge is declared.
	// It is never identity: merged candidates still expand separately.
	ContentFingerprint string `json:"content_fingerprint,omitempty"`
}

// HasSignal reports whether the candidate carries a match signal.
func (c Candidate) HasSignal(s MatchSignal) bool {
	for _, got := range c.MatchSignals {
		if got == s {
			return true
		}
	}
	return false
}

// Exact reports whether this candidate was promoted by an exact identifier
// match. Fusion partitions on this rather than adding a score bonus.
func (c Candidate) Exact() bool { return c.HasSignal(MatchExactIdentifier) }

// Manifest is what an adapter declares about itself at handshake. Identity,
// priors, and policy come from configuration; nothing here names a SourceUID.
type Manifest struct {
	// ProtocolVersion is the negotiated adapter protocol version.
	ProtocolVersion int `json:"protocol_version"`
	// AdapterID is a stable implementation and major-version identifier.
	AdapterID string `json:"adapter_id"`
	// DisplayName is the adapter's human-readable name.
	DisplayName string `json:"display_name"`
	// RecordTypes are the kinds of records the adapter returns.
	RecordTypes []RecordType `json:"record_types"`
	// QueryModes are the retrieval modes the adapter supports.
	QueryModes []QueryMode `json:"query_modes"`
	// FreshnessModes name where the adapter can retrieve from.
	FreshnessModes []FreshnessMode `json:"freshness_modes"`
	// AsOfSupport declares historical-query support.
	AsOfSupport AsOfSupport `json:"as_of_support"`
	// RelevanceBasis declares the quantity placed in Candidate.Relevance.
	// Values on different bases are admissible but not necessarily comparable
	// across sources.
	RelevanceBasis RelevanceBasis `json:"relevance_basis"`

	// DerivesFrom declares that this entire source projects another. It is the
	// fallback lineage edge, used when record-level references are unavailable.
	DerivesFrom string `json:"derives_from,omitempty"`

	// Capabilities declare optional adapter operations.
	Capabilities []Capability `json:"capabilities"`

	// MaxConcurrency limits in-flight requests. Zero means unbounded.
	MaxConcurrency int `json:"max_concurrency,omitempty"`

	// FreshnessPolicy explains how the adapter keeps its view current.
	FreshnessPolicy string `json:"freshness_policy,omitempty"`
	// Sensitivity is the adapter's default data classification.
	Sensitivity Sensitivity `json:"sensitivity"`

	// SettingsSchema validates the instance's adapter-owned settings block.
	SettingsSchema map[string]any `json:"settings_schema,omitempty"`

	// ExecutableRequirements are external programs this configured adapter
	// instance needs in order to serve requests.
	ExecutableRequirements []ExecutableRequirement `json:"executable_requirements,omitempty"`
}

// ExecutableRequirement declares one external program an adapter shells out
// to. Command is the effective configured executable name or path.
type ExecutableRequirement struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Purpose string `json:"purpose,omitempty"`
}

// Supports reports whether the adapter can serve a freshness mode.
func (m Manifest) Supports(mode FreshnessMode) bool {
	for _, got := range m.FreshnessModes {
		if got == mode {
			return true
		}
	}
	return false
}

// Can reports whether the adapter declares a capability.
func (m Manifest) Can(c Capability) bool {
	for _, got := range m.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// Health is an adapter's operational self-report. A recent index timestamp
// alone is not health: coverage and watermarks are what make a partial source
// visible.
type Health struct {
	// Status is the adapter's current availability.
	Status HealthStatus `json:"status"`
	// CheckedAt is when this report was produced.
	CheckedAt time.Time `json:"checked_at"`
	// LastSuccess is the most recent complete successful source boundary.
	LastSuccess *time.Time `json:"last_success_at,omitempty"`

	// SourceWatermark identifies the source state that was inspected.
	SourceWatermark string `json:"source_watermark,omitempty"`
	// IndexWatermark identifies the source state represented by the index.
	IndexWatermark string `json:"index_watermark,omitempty"`
	// IndexGeneration identifies the currently published index generation.
	IndexGeneration string `json:"index_generation,omitempty"`

	// IndexModel identifies the embedding model of the published generation. A
	// model change starts a new generation; one generation never mixes models.
	IndexModel string `json:"index_model,omitempty"`

	// IndexConfig identifies the retrieval configuration the generation was
	// built under: analyzer, tokenizer, scoring parameters. IndexModel covers
	// only embeddings, so without this a change to BM25 constants or stemming
	// silently changes ranking with nothing in the generation recording it —
	// and an evaluation run comparing two generations would attribute the
	// difference to the change under test.
	IndexConfig string `json:"index_config,omitempty"`

	// RecordCount is the number of records in the source boundary.
	RecordCount int64 `json:"record_count,omitempty"`
	// IndexedCount is the number of records represented by the index.
	IndexedCount int64 `json:"indexed_count,omitempty"`
	// FailedCount is the number of records that could not be indexed.
	FailedCount int64 `json:"failed_count,omitempty"`

	// Coverage describes how much of the source the index represents.
	Coverage IndexCoverage `json:"coverage"`

	// Diagnostics carries safe operational detail. It must not reveal record
	// existence for a denied source.
	Diagnostics map[string]any `json:"diagnostics,omitempty"`

	// CheckpointProgress describes comparable checkpoint counts observed
	// before and after the refresh that produced this report. It is omitted
	// for ordinary probes and incomparable boundaries.
	CheckpointProgress CheckpointProgress `json:"checkpoint_progress,omitempty"`

	// ColdStart is how long the adapter took to become ready, when this probe
	// followed a spawn. It counts against the request budget and is reported
	// separately so cold and warm latency stay distinguishable.
	ColdStart time.Duration `json:"cold_start_ns,omitempty"`
}

// CheckpointProgress is the direction of a refresh's comparable checkpoint
// counters. It is separate from health: an operation can advance a partial
// index without making it complete enough for a query.
type CheckpointProgress string

const (
	CheckpointAdvanced  CheckpointProgress = "advanced"
	CheckpointUnchanged CheckpointProgress = "unchanged"
	CheckpointRegressed CheckpointProgress = "regressed"
)

// Usable reports whether a source in this state may be searched.
func (h Health) Usable() bool {
	return h.Status == HealthHealthy || h.Status == HealthDegraded
}

// SearchRequest is what the core asks one adapter. The query text is passed as
// given: term expansion, stemming, and synonyms are source-local concerns, so
// the core synthesizes no variants.
type SearchRequest struct {
	// Query is the caller's unmodified search text.
	Query string `json:"query"`

	// Context is bounded prior message text, honored only by adapters
	// declaring CapContextExpansion.
	Context []string `json:"context,omitempty"`

	// Filters are structured constraints the adapter must honor or refuse.
	Filters Filters `json:"filters"`
	// AsOf is the optional historical boundary.
	AsOf *time.Time `json:"as_of,omitempty"`
	// Limit is the maximum candidates to return.
	Limit int `json:"limit"`

	// Deadline is the absolute time by which search must finish.
	Deadline time.Time `json:"deadline"`
}

// Filters narrows a search before ranking.
type Filters struct {
	// RecordTypes restrict results to these record kinds.
	RecordTypes []RecordType `json:"record_types,omitempty"`
	// Since restricts results to records at or after this time.
	Since *time.Time `json:"since,omitempty"`
	// Until restricts results to records at or before this time.
	Until *time.Time `json:"until,omitempty"`
	// Entities are source-local entity constraints.
	Entities []string `json:"entities,omitempty"`
	// Project is a source-local project constraint.
	Project string `json:"project,omitempty"`
}

// SearchResponse is one source's answer. Outcome is mandatory: a source that
// could not be reached reports it here rather than returning no candidates.
type SearchResponse struct {
	// Candidates are ranked in source-local order.
	Candidates []Candidate `json:"candidates"`
	// Diagnostics carries safe adapter-specific detail.
	Diagnostics map[string]any `json:"diagnostics,omitempty"`
	// SourceWatermark identifies the source state that was searched.
	SourceWatermark string `json:"source_watermark,omitempty"`
	// Outcome states how completely and successfully the search ran.
	Outcome SearchOutcome `json:"outcome"`

	// Reason names why, in the adapter protocol's closed skip-reason vocabulary,
	// when the outcome is [SearchSkipped]. It lets the core decide whether the
	// skip narrowed the request: a source that holds nothing of what was asked
	// for did not degrade anything, and a source that could not evaluate the
	// filter it was given did.
	//
	// Without it an adapter had exactly one way to say "this request does not
	// apply to me" — return success with no candidates — and success asserts a
	// boundary that was crossed and found empty. A project filter naming
	// something no source serves then read as `coverage: complete`, which is
	// the false absence this system exists to prevent, arriving through the one
	// outcome guaranteed not to degrade.
	Reason string `json:"reason,omitempty"`
}

// ExpandRequest retrieves evidence behind a locator.
type ExpandRequest struct {
	// Locator names the source record to retrieve.
	Locator Locator `json:"locator"`
	// Detail selects the requested evidence tier.
	Detail DetailLevel `json:"detail"`
	// Budget is the maximum response size in bytes.
	Budget int64 `json:"budget_bytes"`
	// Deadline is the absolute time by which expansion must finish.
	Deadline time.Time `json:"deadline"`
}

// ExpandResponse is evidence with the provenance needed to trust it.
type ExpandResponse struct {
	// Content is the retrieved evidence.
	Content string `json:"content"`
	// SourceRevision identifies the source version the content came from.
	SourceRevision string `json:"source_revision,omitempty"`
	// Truncated reports whether Content was cut to a limit.
	Truncated bool `json:"truncated"`

	// TruncationBoundary names what limit applied, so a caller can tell a
	// budget cut from a source-side limit.
	TruncationBoundary string `json:"truncation_boundary,omitempty"`

	// Provenance is the path, range, or record reference the content came from.
	Provenance string `json:"provenance,omitempty"`
}
