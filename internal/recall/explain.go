package recall

import "time"

// Explanation is why a result ranked where it did. Its source, rank, signals,
// prior, relevance, lineage, and freshness describe the score-producing
// evidence, which may differ from Result.Primary when a more useful chunk of
// that same document is chosen to represent the record. It is a structure, not
// a rendered string: text output derives from it, and evaluation gates assert
// on its fields.
//
// Every configured value that affected a result appears here. A setting that
// cannot appear in an explanation does not exist, and internal/explain has a
// property test that enforces exactly that.
type Explanation struct {
	SourceUID SourceUID `json:"source_uid"`
	SourceID  string    `json:"source_id"`

	LocalRank     int `json:"local_rank"`
	LocalPoolSize int `json:"local_pool_size"`

	MatchSignals []MatchSignal `json:"match_signals"`

	Prior PriorExplanation `json:"prior"`

	// Relevance is the factor that scaled the prior: the source's estimate of
	// how much this record is about the query.
	//
	// Nil is a third state and not a default, for the same reason
	// [Candidate.ExcerptKind] treats omitted that way: fusion reads a silent
	// source as 1.0, and a value here would make that indistinguishable from a
	// source claiming a perfect match. Those are the same arithmetic and not
	// the same claim, and an explanation that cannot tell them apart has lost
	// the fact a reader most needs when comparing two results.
	Relevance *float64 `json:"relevance,omitempty"`

	LineageRoot   LineageRoot              `json:"lineage_root"`
	Corroboration CorroborationExplanation `json:"corroboration"`
	Freshness     FreshnessExplanation     `json:"freshness"`
	Reranker      RerankerExplanation      `json:"reranker"`

	// ExactPromoted records that the cluster was partitioned above non-exact
	// results. It is a partition, not a score bonus, so it is reported
	// separately from Score.
	ExactPromoted bool `json:"exact_promoted"`

	// Score is the final cluster score, after the corroboration cap.
	Score float64 `json:"score"`

	// RankConstant is the fusion constant in force for this request.
	RankConstant float64 `json:"rank_constant"`
}

// PriorExplanation shows the query-dependent source prior that was applied and
// which configured rule produced it.
type PriorExplanation struct {
	Base float64 `json:"base"`

	// Intent is the bounded adjustment for a named query class, and Rule names
	// the configured rule that fired. An empty Rule with a non-zero Intent is a
	// defect: an adjustment must always be attributable.
	Intent float64 `json:"intent"`
	Rule   string  `json:"rule,omitempty"`

	Effective float64 `json:"effective"`
}

// CorroborationExplanation shows how much independent evidence backs a
// cluster.
//
// The unit is not the lineage root. Two chunks of one document and two
// fingerprint-identical records have distinct roots but are one thing said
// once, so roots first collapse into units and units are what count. A
// three-chunk document is three roots and one unit.
type CorroborationExplanation struct {
	IndependentUnits int      `json:"independent_units"`
	Sources          []string `json:"sources,omitempty"`
	Cap              float64  `json:"cap"`

	// CapApplied means the sum exceeded the cap and was clamped.
	CapApplied bool `json:"cap_applied"`
}

// FreshnessExplanation is the evidence behind a result's currency. A healthy
// index can still be stale, so the generation and its model travel with it.
type FreshnessExplanation struct {
	Mode            FreshnessMode `json:"mode"`
	SourceRevision  string        `json:"source_revision,omitempty"`
	IndexGeneration string        `json:"index_generation,omitempty"`
	IndexModel      string        `json:"index_model,omitempty"`

	// IndexConfig identifies the retrieval configuration the generation was
	// built under. It travels with the result for the same reason it travels
	// with health: a scoring change that nothing records is a ranking change
	// nobody can attribute.
	IndexConfig string     `json:"index_config,omitempty"`
	ObservedAt  *time.Time `json:"observed_at,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`

	// AsOfHonored records how the historical boundary was satisfied, when the
	// request carried one.
	AsOfHonored AsOfSupport `json:"as_of_honored,omitempty"`
}

// RerankerExplanation records whether a shared-scale reranker ran. The
// pre-rerank ordering is always retained, so Delta shows what it changed.
type RerankerExplanation struct {
	Used  bool    `json:"used"`
	Model string  `json:"model,omitempty"`
	Delta float64 `json:"delta,omitempty"`

	// RankBefore is the position this result held before reranking, so a
	// routing mistake hidden by the reranker stays visible.
	RankBefore int `json:"rank_before,omitempty"`
}
