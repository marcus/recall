package recall

import (
	"fmt"
	"strings"
)

// Outcome reports what Recall did with a request. It is orthogonal to
// [Coverage]: a response may abstain with complete coverage, or answer with
// degraded coverage.
type Outcome string

const (
	// OutcomeAnswered means Recall returned at least one result.
	OutcomeAnswered Outcome = "answered"
	// OutcomeAbstained means Recall searched successfully but found no answer.
	OutcomeAbstained Outcome = "abstained"
	// OutcomeFailed means Recall could not produce a trustworthy answer.
	OutcomeFailed Outcome = "failed"
)

// Coverage reports whether every eligible source was searched. Truncation of
// results to fit a token budget is not degradation; see
// [QueryResponse.Truncated].
type Coverage string

const (
	// CoverageComplete means every eligible source was searched.
	CoverageComplete Coverage = "complete"
	// CoverageDegraded means at least one eligible source did not complete.
	CoverageDegraded Coverage = "degraded"
)

// SearchOutcome is one source's result for one search. An unavailable source
// never reports success with zero candidates.
type SearchOutcome string

const (
	// SearchSuccess means the adapter searched its complete source boundary.
	SearchSuccess SearchOutcome = "success"
	// SearchPartial means the adapter searched only part of its source boundary.
	SearchPartial SearchOutcome = "partial"
	// SearchUnavailable means the source could not be reached.
	SearchUnavailable SearchOutcome = "unavailable"
	// SearchDenied means policy or credentials denied access to the source.
	SearchDenied SearchOutcome = "denied"
	// SearchFailed means the adapter failed while searching.
	SearchFailed SearchOutcome = "failed"
	// SearchTimeout means the search exceeded its deadline.
	SearchTimeout SearchOutcome = "timeout"
	// SearchSkipped means the source did not search: the core found it
	// ineligible, its deadline had already elapsed, or the ADAPTER said the
	// request named something it does not serve.
	//
	// The last of those is why an adapter may return it. Before, the only way
	// to say "this does not apply to me" was success with no candidates, and
	// success asserts a boundary that was crossed and found empty. A project
	// filter naming a workspace nobody has therefore read as complete coverage
	// over a search that never ran. See [SearchResponse.Reason].
	SearchSkipped SearchOutcome = "skipped"
)

// Skip reasons an adapter may state alongside [SearchSkipped].
//
// They live here, beside the outcome they qualify, because they are part of the
// search contract: an adapter must be able to name one without importing the
// core's planner, and an external adapter names one over the wire. The core
// decides what each costs; the adapter only reports which is true.
const (
	// SkipNotApplicable is the request naming something this source does not
	// serve — a project it is not, an entity class it does not hold. Only the
	// source knows what it answers to, so the core cannot derive it.
	//
	// It does not degrade on its own: one source not being the one asked for
	// is routing working as intended. What must not follow from it is complete
	// coverage when NO source was applicable, which is judged over the whole
	// response rather than per source.
	SkipNotApplicable = "not_applicable"

	// SkipFilterUnsupported is the adapter saying it could not EVALUATE a
	// filter, as opposed to evaluating it and matching nothing. What it would
	// return is a superset or a subset of what was asked for and it cannot say
	// which, so this degrades.
	SkipFilterUnsupported = "filter_unsupported"

	// SkipRecordTypeMismatch is the request asking for record types this source
	// does not hold. The core already excludes on record type before a search,
	// so an adapter reaching for this is refusing again rather than first — but
	// it is the honest name when it does, and it does not degrade.
	SkipRecordTypeMismatch = "record_type_mismatch"

	// SkipUnstated is what a skip without a reason is treated as. It degrades:
	// an unexplained absence is precisely what must not be reported as a
	// boundary that was crossed.
	SkipUnstated = "skip_unstated"
)

// Searched reports whether the outcome represents a source that actually ran.
func (o SearchOutcome) Searched() bool {
	return o == SearchSuccess || o == SearchPartial
}

// Degrades reports whether this outcome forces [CoverageDegraded].
func (o SearchOutcome) Degrades() bool {
	return o != SearchSuccess
}

// HealthStatus is an adapter's self-report. It uses a different vocabulary
// from [SearchOutcome] deliberately: health describes a source over time,
// a search outcome describes one request.
type HealthStatus string

const (
	// HealthHealthy means the adapter can search its complete source boundary.
	HealthHealthy HealthStatus = "healthy"
	// HealthDegraded means the adapter can search only a partial or stale view.
	HealthDegraded HealthStatus = "degraded"
	// HealthUnavailable means the adapter cannot reach its source.
	HealthUnavailable HealthStatus = "unavailable"
	// HealthDenied means the adapter lacks permission to inspect its source.
	HealthDenied HealthStatus = "denied"
)

// IndexCoverage describes how much of a source the current index represents.
// Distinct from [Coverage], which describes one response.
type IndexCoverage string

const (
	// IndexComplete means the index represents the complete source boundary.
	IndexComplete IndexCoverage = "complete"
	// IndexPartial means the index represents only part of the source boundary.
	IndexPartial IndexCoverage = "partial"
	// IndexUnknown means the adapter cannot establish index coverage.
	IndexUnknown IndexCoverage = "unknown"
)

// MatchSignal explains why an adapter surfaced a candidate.
type MatchSignal string

const (
	// MatchLexical means the query matched record text.
	MatchLexical MatchSignal = "lexical"
	// MatchSemantic means semantic retrieval matched the record.
	MatchSemantic MatchSignal = "semantic"
	// MatchField means the query matched a structured field.
	MatchField MatchSignal = "field"
	// MatchAlias means the query matched a declared alias.
	MatchAlias MatchSignal = "alias"

	// MatchExactIdentifier is emitted only for an exact match on a stable
	// identifier or declared alias, at token boundaries. Unbounded substring
	// matches never carry it. It drives exact-match promotion in fusion.
	MatchExactIdentifier MatchSignal = "exact_identifier"
)

// ExcerptKind says what a candidate's excerpt is evidence of.
//
// The two values are not interchangeable. A matched excerpt is the reason the
// record was returned; a preview is the record's opening, shown because nothing
// in its text matched — the query named the document outright, or matched a
// field the excerpt does not carry. A caller that cannot tell them apart has to
// re-derive the match by hand to find out whether a hit was real.
//
// The empty value is neither, and is not a default standing in for preview: it
// says the source made no claim. A source that does not select excerpts by
// query leaves it empty, and so does one that could not read the record to find
// out. Reporting either of those as a preview would assert that nothing
// matched, which is a claim about the record rather than about the source.
type ExcerptKind string

const (
	// ExcerptMatched means the excerpt is the span the query matched.
	ExcerptMatched ExcerptKind = "matched"
	// ExcerptPreview means nothing in the record's text matched and the
	// excerpt is its opening.
	ExcerptPreview ExcerptKind = "preview"
)

// QueryMode is a kind of retrieval an adapter supports.
type QueryMode string

const (
	// QueryExact means the adapter can retrieve by exact identifier.
	QueryExact QueryMode = "exact"
	// QueryLexical means the adapter can retrieve by lexical matching.
	QueryLexical QueryMode = "lexical"
	// QuerySemantic means the adapter can retrieve by semantic similarity.
	QuerySemantic QueryMode = "semantic"
	// QueryStructured means the adapter can apply structured constraints.
	QueryStructured QueryMode = "structured"
	// QueryTemporal means the adapter can retrieve by time.
	QueryTemporal QueryMode = "temporal"
)

// FreshnessMode is how a source instance answers: from the source of truth,
// from an adapter-owned projection, or both.
type FreshnessMode string

const (
	// FreshnessLive means the adapter reads the source of truth per request.
	FreshnessLive FreshnessMode = "live"
	// FreshnessIndexed means the adapter answers from its own projection.
	FreshnessIndexed FreshnessMode = "indexed"
	// FreshnessHybrid means the adapter combines live and indexed retrieval.
	FreshnessHybrid FreshnessMode = "hybrid"
)

// AsOfSupport declares how far an adapter can honor a historical boundary. A
// source declaring AsOfNone is excluded from a request carrying as_of; it
// never answers a historical question from current state.
type AsOfSupport string

const (
	// AsOfNone means the adapter cannot honor a historical boundary.
	AsOfNone AsOfSupport = "none"
	// AsOfFilter means the adapter can filter records by effective time.
	AsOfFilter AsOfSupport = "filter"
	// AsOfSnapshot means the adapter can reconstruct source state at a time.
	AsOfSnapshot AsOfSupport = "snapshot"
)

// Honors reports whether this level of support can answer a request carrying
// an as_of boundary.
func (a AsOfSupport) Honors() bool { return a == AsOfFilter || a == AsOfSnapshot }

// RecordType names the kind of thing a record is. The set is open: adapters
// may declare types the core has never seen, and the core only compares them.
type RecordType string

const (
	// RecordPerson is a person record.
	RecordPerson RecordType = "person"
	// RecordTask is a task record.
	RecordTask RecordType = "task"
	// RecordDocument is a document record.
	RecordDocument RecordType = "document"
	// RecordMessage is a message record.
	RecordMessage RecordType = "message"
	// RecordEvent is an event record.
	RecordEvent RecordType = "event"
)

// DetailLevel selects how much evidence an expansion returns.
type DetailLevel string

const (
	// DetailSummary asks for a compact summary.
	DetailSummary DetailLevel = "summary"
	// DetailExcerpt asks for the relevant excerpt.
	DetailExcerpt DetailLevel = "excerpt"
	// DetailFull asks for the complete record within the byte budget.
	DetailFull DetailLevel = "full"
	// DetailContext asks for the record with surrounding context.
	DetailContext DetailLevel = "context"
)

// InvocationMode distinguishes a user-visible request from a host's pre-reply
// budget. Suppression applies only to pre-reply.
type InvocationMode string

const (
	// ModeExplicit is a user-visible recall request.
	ModeExplicit InvocationMode = "explicit"
	// ModePreReply is a host's bounded recall before composing a reply.
	ModePreReply InvocationMode = "pre_reply"
)

// Capability names an optional adapter behavior declared in its manifest.
type Capability string

const (
	// CapSearch means the adapter implements search.
	CapSearch Capability = "search"
	// CapExpand means the adapter expands locators into evidence.
	CapExpand Capability = "expand"
	// CapEnumerate means the adapter can enumerate its source.
	CapEnumerate Capability = "enumerate"
	// CapCheckpoint means the adapter supports refreshable checkpoints.
	CapCheckpoint Capability = "checkpoint"
	// CapContextExpansion means search can use prior message context.
	CapContextExpansion Capability = "context_expansion"
)

// Sensitivity is an ordered data classification. Comparison is total, so a
// ceiling check is unambiguous everywhere it is applied.
type Sensitivity int

const (
	// SensitivityPublic is suitable for unrestricted disclosure.
	SensitivityPublic Sensitivity = iota
	// SensitivityInternal is suitable only for the owning organization or user.
	SensitivityInternal
	// SensitivityConfidential requires an explicitly trusted caller.
	SensitivityConfidential
	// SensitivityRestricted is the most restrictive classification.
	SensitivityRestricted
)

var sensitivityNames = [...]string{"public", "internal", "confidential", "restricted"}

func (s Sensitivity) String() string {
	if s < 0 || int(s) >= len(sensitivityNames) {
		return fmt.Sprintf("sensitivity(%d)", int(s))
	}
	return sensitivityNames[s]
}

// Valid reports whether s is a defined level.
func (s Sensitivity) Valid() bool { return s >= 0 && int(s) < len(sensitivityNames) }

// AtMost reports whether s is permitted by a ceiling.
func (s Sensitivity) AtMost(ceiling Sensitivity) bool { return s <= ceiling }

// Raise returns the more restrictive of two levels. An adapter may raise a
// candidate's classification above its source floor but never lower it, so
// every combination of source floor and candidate label goes through here.
func (s Sensitivity) Raise(other Sensitivity) Sensitivity {
	if other > s {
		return other
	}
	return s
}

// ParseSensitivity accepts a level name.
func ParseSensitivity(s string) (Sensitivity, error) {
	for i, name := range sensitivityNames {
		if strings.EqualFold(s, name) {
			return Sensitivity(i), nil
		}
	}
	return 0, fmt.Errorf("unknown sensitivity %q, want one of %s",
		s, strings.Join(sensitivityNames[:], ", "))
}

// MarshalText renders the level name, so JSON carries "internal" and not 1.
func (s Sensitivity) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("refusing to marshal invalid %s", s)
	}
	return []byte(s.String()), nil
}

// UnmarshalText parses a sensitivity level name.
func (s *Sensitivity) UnmarshalText(b []byte) error {
	parsed, err := ParseSensitivity(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
