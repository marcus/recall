package eval

import (
	"fmt"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// SchemaVersion is the pack, case, judgment, and run schema version this build
// writes and understands. A pack declaring anything else is refused rather
// than read on a guess: an older reader silently ignoring a newer field would
// report metrics over content it did not fully understand.
const SchemaVersion = 1

// PackFile is the manifest's name inside a pack directory.
const PackFile = "pack.json"

// Pack is a pack manifest: what the pack is, where its cases and judgments
// live, and the budgets and thresholds a run is held to. It never contains
// cases or judgments itself.
type Pack struct {
	SchemaVersion int    `json:"schema_version"`
	PackID        string `json:"pack_id"`
	Version       string `json:"version"`
	Description   string `json:"description,omitempty"`

	// Profile is the profile the pack expects to be resolved against.
	Profile string `json:"profile,omitempty"`

	// Cases and Judgments are JSONL paths relative to the pack directory. They
	// are two paths and never one: the runner reads Cases to build the request
	// and Judgments only to score the answer.
	Cases     string `json:"cases"`
	Judgments string `json:"judgments"`

	Sources     string `json:"sources,omitempty"`
	Transcripts string `json:"transcripts,omitempty"`

	// NetworkAccess is false for every pack but a live health pack. It is
	// declared rather than assumed so an undeclared network call is a gate
	// failure and not a surprise.
	NetworkAccess bool `json:"network_access,omitempty"`

	Budgets    *Budgets           `json:"budgets,omitempty"`
	Thresholds map[string]float64 `json:"thresholds,omitempty"`

	// SensitivityCeiling is a pointer because the zero Sensitivity is a real
	// level ("public"); an undeclared ceiling and a public ceiling are
	// different statements.
	SensitivityCeiling *recall.Sensitivity `json:"sensitivity_ceiling,omitempty"`

	// Baseline records where the thresholds came from. A threshold with no
	// record of when it was measured, and on what, is folklore — and the first
	// question anyone asks of a failing gate is whether the bar was ever
	// justified.
	Baseline *Baseline `json:"baseline,omitempty"`

	// dir is where the manifest was read from. It is not part of the pack's
	// content: a pack moved to another path is the same pack.
	dir string
}

// Dir reports the directory the pack was loaded from.
func (p *Pack) Dir() string { return p.dir }

// Baseline is the provenance of a pack's thresholds.
type Baseline struct {
	Recorded string `json:"recorded"`
	Note     string `json:"note,omitempty"`
}

// Budgets bounds one run. A run exceeding any of these is invalid, not slow.
type Budgets struct {
	P95LatencyMS     int `json:"p95_latency_ms,omitempty"`
	ModelCalls       int `json:"model_calls,omitempty"`
	Tokens           int `json:"tokens,omitempty"`
	ExternalRequests int `json:"external_requests,omitempty"`
}

// Behavior is what a case expects Recall to do. It is not a relevance grade:
// correctly abstaining is a success, and the metric that measures it is
// separate from every ranking metric.
type Behavior string

// The vocabulary matches recall.Outcome one for one, deliberately. An earlier
// draft had a "clarify" behavior with no outcome behind it, so a case expecting
// it could never pass: Recall retrieves, and deciding to ask a follow-up
// question is something a host does with what Recall returned.
const (
	BehaviorAnswer  Behavior = "answer"
	BehaviorAbstain Behavior = "abstain"
	BehaviorFail    Behavior = "fail"
)

// Valid reports whether b is a defined behavior.
func (b Behavior) Valid() bool {
	switch b {
	case BehaviorAnswer, BehaviorAbstain, BehaviorFail:
		return true
	default:
		return false
	}
}

// Case is one query put to Recall, plus what the run must report about it.
//
// A Case carries no answers. Adding a relevance field here would let a runner
// leak ground truth to the agent it is evaluating simply by passing the case
// through, so relevance lives in [Judgment] and is loaded by a separate call.
type Case struct {
	SchemaVersion int    `json:"schema_version"`
	CaseID        string `json:"case_id"`
	Query         string `json:"query"`
	Profile       string `json:"profile"`

	// AsOf bounds the case to a past instant. Fixture runs inject the clock
	// from it, so a case with an as_of is reproducible forever.
	AsOf *time.Time `json:"as_of,omitempty"`

	ExpectedBehavior Behavior `json:"expected_behavior"`

	// Tags are reporting groups. A case belongs to every tag it carries.
	Tags []string `json:"tags,omitempty"`

	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Notes     string `json:"notes,omitempty"`

	// Mode is the invocation mode. It defaults to explicit; a case testing
	// suppression must say pre_reply, because suppression applies to a host's
	// passive budget and never hides evidence somebody asked for.
	Mode recall.InvocationMode `json:"mode,omitempty"`

	// SuppressLineages is what the host has already shown. Suppression state
	// belongs to the host, so a case that tests it supplies the state.
	SuppressLineages []recall.LineageRoot `json:"suppress_lineages,omitempty"`

	// Scope narrows the request the way a caller would.
	Scope *CaseScope `json:"scope,omitempty"`

	Assertions *Assertions `json:"assertions,omitempty"`
}

// CaseScope is the request scope a case sets.
type CaseScope struct {
	SourceIDs   []string            `json:"source_ids,omitempty"`
	RecordTypes []recall.RecordType `json:"record_types,omitempty"`
	Entities    []string            `json:"entities,omitempty"`
	Project     string              `json:"project,omitempty"`
}

// Assertions are the policy claims a case makes. They are not relevance
// judgments: they state what the run must report, not which evidence is good.
type Assertions struct {
	// ExpectedCoverage is the ground truth behind coverage accuracy. Degraded
	// must be reported when and only when this says degraded.
	ExpectedCoverage recall.Coverage `json:"expected_coverage,omitempty"`

	ExpectedSourceOutcomes map[recall.SourceUID]recall.SearchOutcome `json:"expected_source_outcomes,omitempty"`

	RequiredSources  []recall.SourceUID `json:"required_sources,omitempty"`
	ForbiddenSources []recall.SourceUID `json:"forbidden_sources,omitempty"`

	MaxLatencyMS      int   `json:"max_latency_ms,omitempty"`
	MaxExpansionBytes int64 `json:"max_expansion_bytes,omitempty"`

	// ExpectedRevisions demands that a returned locator for a lineage root
	// expands to a named source revision. Resolving to the right record at the
	// wrong revision is a locator failure, not a near miss.
	ExpectedRevisions map[recall.LineageRoot]string `json:"expected_revisions,omitempty"`

	SuppressedLineages []recall.LineageRoot `json:"suppressed_lineages,omitempty"`
	VisibleLineages    []recall.LineageRoot `json:"visible_lineages,omitempty"`

	SensitivityCeiling *recall.Sensitivity `json:"sensitivity_ceiling,omitempty"`
}

// Relevance is the graded judgment vocabulary. The set is closed: a grade
// outside it is a pack defect, because every graded metric weights by the
// numeric value and an unknown grade would silently change a score.
type Relevance int

const (
	Irrelevant     Relevance = 0
	RelatedContext Relevance = 1
	UsefulSupport  Relevance = 2
	Authoritative  Relevance = 3
)

var relevanceNames = [...]string{"irrelevant", "related_context", "useful_support", "authoritative"}

// Valid reports whether r is a defined grade.
func (r Relevance) Valid() bool { return r >= Irrelevant && int(r) < len(relevanceNames) }

func (r Relevance) String() string {
	if !r.Valid() {
		return fmt.Sprintf("relevance(%d)", int(r))
	}
	return relevanceNames[r]
}

// Judgment is one graded judgment for one case.
//
// It targets an evidence lineage, not rendered text: two candidates projecting
// one record share a lineage root and are one piece of evidence. The root is a
// persisted-form locator, so it keys on source_uid and renaming a source does
// not invalidate the pack.
type Judgment struct {
	SchemaVersion int                `json:"schema_version"`
	CaseID        string             `json:"case_id"`
	LineageRoot   recall.LineageRoot `json:"lineage_root"`
	Relevance     Relevance          `json:"relevance"`

	// Required marks evidence the case cannot be answered without. Recall@k is
	// measured over these, which is why it is a flag and not a grade: the most
	// authoritative record is not always the one that must be found.
	Required bool `json:"required,omitempty"`

	// Forbidden marks evidence that must not appear near the top: superseded,
	// out of scope, or over the sensitivity ceiling.
	Forbidden bool `json:"forbidden,omitempty"`

	Supports []string `json:"supports,omitempty"`
}
