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

	SourceUID SourceUID `json:"source_uid"`
	SourceID  string    `json:"source_id"`

	// SourceRecordID is the native or adapter-defined record identity.
	SourceRecordID string `json:"source_record_id"`

	Locator Locator `json:"locator"`

	// DerivedFrom names upstream records this candidate projects. It is the
	// primary lineage edge: a signal projecting a task declares that task's
	// locator, and the two never corroborate each other.
	DerivedFrom []Locator `json:"derived_from,omitempty"`

	RecordType RecordType `json:"record_type"`
	Title      string     `json:"title"`
	Excerpt    string     `json:"excerpt,omitempty"`

	// ExcerptKind reports whether the excerpt is the span that matched or the
	// record's opening. Empty is a third state and not a default: the source
	// asserts nothing, either because it does not select excerpts by query or
	// because it could not establish which this one is.
	ExcerptKind ExcerptKind `json:"excerpt_kind,omitempty"`

	// LocalRank is the candidate's position in its source's own result list,
	// one-based. It is the only mandatory relevance signal and the only one
	// fusion consumes.
	LocalRank int `json:"local_rank"`

	// LocalScore is the source's native score. It is diagnostic: scales differ
	// between engines, so it is never compared across sources.
	LocalScore *float64 `json:"local_score,omitempty"`

	MatchSignals []MatchSignal `json:"match_signals,omitempty"`

	// The four timestamps below answer different questions and are never
	// collapsed. ObservedAt is when Recall read the record; ConfirmedAt is when
	// a complete successful source boundary confirmed it; EventTime is when the
	// underlying event happened; ValidFrom/ValidTo bound when a fact was true.
	ObservedAt  *time.Time `json:"observed_at,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	EventTime   *time.Time `json:"event_time,omitempty"`
	ValidFrom   *time.Time `json:"valid_from,omitempty"`
	ValidTo     *time.Time `json:"valid_to,omitempty"`

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
	ProtocolVersion int             `json:"protocol_version"`
	AdapterID       string          `json:"adapter_id"`
	DisplayName     string          `json:"display_name"`
	RecordTypes     []RecordType    `json:"record_types"`
	QueryModes      []QueryMode     `json:"query_modes"`
	FreshnessModes  []FreshnessMode `json:"freshness_modes"`
	AsOfSupport     AsOfSupport     `json:"as_of_support"`

	// DerivesFrom declares that this entire source projects another. It is the
	// fallback lineage edge, used when record-level references are unavailable.
	DerivesFrom string `json:"derives_from,omitempty"`

	Capabilities []Capability `json:"capabilities"`

	// MaxConcurrency limits in-flight requests. Zero means unbounded.
	MaxConcurrency int `json:"max_concurrency,omitempty"`

	FreshnessPolicy string      `json:"freshness_policy,omitempty"`
	Sensitivity     Sensitivity `json:"sensitivity"`

	// SettingsSchema validates the instance's adapter-owned settings block.
	SettingsSchema map[string]any `json:"settings_schema,omitempty"`
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
	Status      HealthStatus `json:"status"`
	CheckedAt   time.Time    `json:"checked_at"`
	LastSuccess *time.Time   `json:"last_success_at,omitempty"`

	SourceWatermark string `json:"source_watermark,omitempty"`
	IndexWatermark  string `json:"index_watermark,omitempty"`
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

	RecordCount  int64 `json:"record_count,omitempty"`
	IndexedCount int64 `json:"indexed_count,omitempty"`
	FailedCount  int64 `json:"failed_count,omitempty"`

	Coverage IndexCoverage `json:"coverage"`

	// Diagnostics carries safe operational detail. It must not reveal record
	// existence for a denied source.
	Diagnostics map[string]any `json:"diagnostics,omitempty"`

	// ColdStart is how long the adapter took to become ready, when this probe
	// followed a spawn. It counts against the request budget and is reported
	// separately so cold and warm latency stay distinguishable.
	ColdStart time.Duration `json:"cold_start_ns,omitempty"`
}

// Usable reports whether a source in this state may be searched.
func (h Health) Usable() bool {
	return h.Status == HealthHealthy || h.Status == HealthDegraded
}

// SearchRequest is what the core asks one adapter. The query text is passed as
// given: term expansion, stemming, and synonyms are source-local concerns, so
// the core synthesizes no variants.
type SearchRequest struct {
	Query string `json:"query"`

	// Context is bounded prior message text, honored only by adapters
	// declaring CapContextExpansion.
	Context []string `json:"context,omitempty"`

	Filters Filters    `json:"filters"`
	AsOf    *time.Time `json:"as_of,omitempty"`
	Limit   int        `json:"limit"`

	Deadline time.Time `json:"deadline"`
}

// Filters narrows a search before ranking.
type Filters struct {
	RecordTypes []RecordType `json:"record_types,omitempty"`
	Since       *time.Time   `json:"since,omitempty"`
	Until       *time.Time   `json:"until,omitempty"`
	Entities    []string     `json:"entities,omitempty"`
	Project     string       `json:"project,omitempty"`
}

// SearchResponse is one source's answer. Outcome is mandatory: a source that
// could not be reached reports it here rather than returning no candidates.
type SearchResponse struct {
	Candidates      []Candidate    `json:"candidates"`
	Diagnostics     map[string]any `json:"diagnostics,omitempty"`
	SourceWatermark string         `json:"source_watermark,omitempty"`
	Outcome         SearchOutcome  `json:"outcome"`

	// Reason names why, in the closed vocabulary of internal/source, when the
	// outcome is [SearchSkipped]. It is what lets the core decide whether the
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
	Locator  Locator     `json:"locator"`
	Detail   DetailLevel `json:"detail"`
	Budget   int64       `json:"budget_bytes"`
	Deadline time.Time   `json:"deadline"`
}

// ExpandResponse is evidence with the provenance needed to trust it.
type ExpandResponse struct {
	Content        string `json:"content"`
	SourceRevision string `json:"source_revision,omitempty"`
	Truncated      bool   `json:"truncated"`

	// TruncationBoundary names what limit applied, so a caller can tell a
	// budget cut from a source-side limit.
	TruncationBoundary string `json:"truncation_boundary,omitempty"`

	// Provenance is the path, range, or record reference the content came from.
	Provenance string `json:"provenance,omitempty"`
}
