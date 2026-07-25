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
	OutcomeAnswered  Outcome = "answered"
	OutcomeAbstained Outcome = "abstained"
	OutcomeFailed    Outcome = "failed"
)

// Coverage reports whether every eligible source was searched. Truncation of
// results to fit a token budget is not degradation; see
// [QueryResponse.Truncated].
type Coverage string

const (
	CoverageComplete Coverage = "complete"
	CoverageDegraded Coverage = "degraded"
)

// SearchOutcome is one source's result for one search. An unavailable source
// never reports success with zero candidates.
type SearchOutcome string

const (
	SearchSuccess     SearchOutcome = "success"
	SearchPartial     SearchOutcome = "partial"
	SearchUnavailable SearchOutcome = "unavailable"
	SearchDenied      SearchOutcome = "denied"
	SearchFailed      SearchOutcome = "failed"
	SearchTimeout     SearchOutcome = "timeout"
	// SearchSkipped means the source was never asked: it was ineligible, or
	// its deadline had already elapsed.
	SearchSkipped SearchOutcome = "skipped"
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
	HealthHealthy     HealthStatus = "healthy"
	HealthDegraded    HealthStatus = "degraded"
	HealthUnavailable HealthStatus = "unavailable"
	HealthDenied      HealthStatus = "denied"
)

// IndexCoverage describes how much of a source the current index represents.
// Distinct from [Coverage], which describes one response.
type IndexCoverage string

const (
	IndexComplete IndexCoverage = "complete"
	IndexPartial  IndexCoverage = "partial"
	IndexUnknown  IndexCoverage = "unknown"
)

// MatchSignal explains why an adapter surfaced a candidate.
type MatchSignal string

const (
	// MatchExactIdentifier is emitted only for an exact match on a stable
	// identifier or declared alias, at token boundaries. Unbounded substring
	// matches never carry it. It drives exact-match promotion in fusion.
	MatchExactIdentifier MatchSignal = "exact_identifier"
	MatchLexical         MatchSignal = "lexical"
	MatchSemantic        MatchSignal = "semantic"
	MatchField           MatchSignal = "field"
	MatchAlias           MatchSignal = "alias"
)

// QueryMode is a kind of retrieval an adapter supports.
type QueryMode string

const (
	QueryExact      QueryMode = "exact"
	QueryLexical    QueryMode = "lexical"
	QuerySemantic   QueryMode = "semantic"
	QueryStructured QueryMode = "structured"
	QueryTemporal   QueryMode = "temporal"
)

// FreshnessMode is how a source instance answers: from the source of truth,
// from an adapter-owned projection, or both.
type FreshnessMode string

const (
	FreshnessLive    FreshnessMode = "live"
	FreshnessIndexed FreshnessMode = "indexed"
	FreshnessHybrid  FreshnessMode = "hybrid"
)

// AsOfSupport declares how far an adapter can honor a historical boundary. A
// source declaring AsOfNone is excluded from a request carrying as_of; it
// never answers a historical question from current state.
type AsOfSupport string

const (
	AsOfNone     AsOfSupport = "none"
	AsOfFilter   AsOfSupport = "filter"
	AsOfSnapshot AsOfSupport = "snapshot"
)

// Honors reports whether this level of support can answer a request carrying
// an as_of boundary.
func (a AsOfSupport) Honors() bool { return a == AsOfFilter || a == AsOfSnapshot }

// RecordType names the kind of thing a record is. The set is open: adapters
// may declare types the core has never seen, and the core only compares them.
type RecordType string

const (
	RecordPerson   RecordType = "person"
	RecordTask     RecordType = "task"
	RecordDocument RecordType = "document"
	RecordMessage  RecordType = "message"
	RecordEvent    RecordType = "event"
)

// DetailLevel selects how much evidence an expansion returns.
type DetailLevel string

const (
	DetailSummary DetailLevel = "summary"
	DetailExcerpt DetailLevel = "excerpt"
	DetailFull    DetailLevel = "full"
	DetailContext DetailLevel = "context"
)

// InvocationMode distinguishes a user-visible request from a host's pre-reply
// budget. Suppression applies only to pre-reply.
type InvocationMode string

const (
	ModeExplicit InvocationMode = "explicit"
	ModePreReply InvocationMode = "pre_reply"
)

// Capability names an optional adapter behavior declared in its manifest.
type Capability string

const (
	CapSearch           Capability = "search"
	CapExpand           Capability = "expand"
	CapEnumerate        Capability = "enumerate"
	CapCheckpoint       Capability = "checkpoint"
	CapContextExpansion Capability = "context_expansion"
)

// Sensitivity is an ordered data classification. Comparison is total, so a
// ceiling check is unambiguous everywhere it is applied.
type Sensitivity int

const (
	SensitivityPublic Sensitivity = iota
	SensitivityInternal
	SensitivityConfidential
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

func (s *Sensitivity) UnmarshalText(b []byte) error {
	parsed, err := ParseSensitivity(string(b))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
