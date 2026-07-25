package recall

import "time"

// Explanation is why a result ranked where it did. It is a structure, not a
// rendered string: text output derives from it, and evaluation gates assert on
// its fields.
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

	Prior         PriorExplanation         `json:"prior"`
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

	Suppressions []string `json:"suppressions,omitempty"`
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

// CorroborationExplanation shows how many independent records back a cluster.
// Only distinct lineage roots count; two projections of one record are one
// piece of evidence.
type CorroborationExplanation struct {
	DistinctLineages int      `json:"distinct_lineages"`
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
	ObservedAt      *time.Time    `json:"observed_at,omitempty"`
	ConfirmedAt     *time.Time    `json:"confirmed_at,omitempty"`

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
