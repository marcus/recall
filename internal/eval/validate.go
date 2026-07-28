package eval

import (
	"errors"
	"fmt"

	"github.com/marcus/recall/internal/recall"
)

// Errors Validate reports. They are sentinels because `recall eval validate`
// is a gate: a caller decides what to do about a pack by what is wrong with
// it, not by matching on message text.
var (
	// ErrUnsupportedSchema means the pack declares a schema version this build
	// does not implement. Reading it anyway would measure content the reader
	// only partly understood.
	ErrUnsupportedSchema = errors.New("unsupported schema_version")

	// ErrUnknownCase means a judgment scores a case that does not exist. Left
	// unchecked it is invisible: the judgment simply never applies, and a
	// metric quietly loses its ground truth.
	ErrUnknownCase = errors.New("judgment references unknown case_id")

	// ErrUnknownGrade means a relevance value outside the defined vocabulary.
	// Graded metrics weight by the numeric value, so an unknown grade silently
	// changes a score rather than failing.
	ErrUnknownGrade = errors.New("unknown relevance grade")

	// ErrMalformedLineageRoot means a lineage root that is not a persisted-form
	// locator. A root that cannot be parsed cannot be matched against a
	// candidate's, so the judgment would never fire.
	ErrMalformedLineageRoot = errors.New("malformed lineage_root")

	// ErrDuplicateCase means two cases share an ID, which makes judgments
	// ambiguous.
	ErrDuplicateCase = errors.New("duplicate case_id")

	// ErrDuplicateJudgment means one case grades one lineage root twice.
	ErrDuplicateJudgment = errors.New("duplicate judgment")

	// ErrContradictoryJudgment means evidence marked both required and
	// forbidden.
	ErrContradictoryJudgment = errors.New("judgment is both required and forbidden")

	// ErrInvalidBehavior means an expected behavior outside answer, clarify,
	// abstain.
	ErrInvalidBehavior = errors.New("unknown expected_behavior")

	// ErrUnsupportedThreshold means a manifest declared a gate threshold that
	// this evaluator never consults.
	ErrUnsupportedThreshold = errors.New("unsupported threshold")

	// ErrVacuousExpectedFail means an expected-failure marking names an
	// assertion the case does not declare. Nothing can violate an assertion
	// that was never stated, so the marking would be permanent decoration
	// naming a defect nothing measures.
	ErrVacuousExpectedFail = errors.New("expected_fail names an assertion the case does not declare")

	// ErrUnknownAssertionName means an expected-failure marking names
	// something outside the assertion vocabulary. It would excuse nothing and
	// go stale immediately, which is a typo reported as a defect fix.
	ErrUnknownAssertionName = errors.New("unknown assertion name")

	// ErrImpossibleResultBounds means min_results exceeds max_results. No run
	// can satisfy both, so the case would fail forever for a reason that is in
	// the pack rather than in the system.
	ErrImpossibleResultBounds = errors.New("min_results exceeds max_results")
)

var supportedThresholds = map[string]bool{
	"exact_identifier_success_at_1": true,
	"abstention_accuracy":           true,
}

// Validate checks a pack's manifest, cases, and judgments against each other.
//
// It is the layer that sees what a single-document schema cannot: whether a
// judgment's case exists, whether a case is graded twice, whether this build
// implements the declared schema version. Every problem found is reported, not
// just the first, because a pack is edited by hand and one round trip per
// mistake is a bad way to spend a morning.
func Validate(pack *Pack, cases []Case, judgments []Judgment) error {
	var problems []error

	if pack == nil {
		return fmt.Errorf("%w: no manifest", ErrUnsupportedSchema)
	}
	if pack.SchemaVersion != SchemaVersion {
		problems = append(problems, fmt.Errorf("pack %q: %w %d, this build implements %d",
			pack.PackID, ErrUnsupportedSchema, pack.SchemaVersion, SchemaVersion))
	}
	for name := range pack.Thresholds {
		if !supportedThresholds[name] {
			problems = append(problems, fmt.Errorf("pack %q: %w %q",
				pack.PackID, ErrUnsupportedThreshold, name))
		}
	}

	known := make(map[string]bool, len(cases))
	for _, c := range cases {
		if known[c.CaseID] {
			problems = append(problems, fmt.Errorf("case %q: %w", c.CaseID, ErrDuplicateCase))
		}
		known[c.CaseID] = true

		if c.SchemaVersion != SchemaVersion {
			problems = append(problems, fmt.Errorf("case %q: %w %d, this build implements %d",
				c.CaseID, ErrUnsupportedSchema, c.SchemaVersion, SchemaVersion))
		}
		if !c.ExpectedBehavior.Valid() {
			problems = append(problems, fmt.Errorf("case %q: %w %q",
				c.CaseID, ErrInvalidBehavior, c.ExpectedBehavior))
		}
		problems = append(problems, checkAssertions(c)...)
	}

	seen := make(map[string]bool, len(judgments))
	for _, j := range judgments {
		where := fmt.Sprintf("judgment %q/%q", j.CaseID, j.LineageRoot)

		if j.SchemaVersion != SchemaVersion {
			problems = append(problems, fmt.Errorf("%s: %w %d, this build implements %d",
				where, ErrUnsupportedSchema, j.SchemaVersion, SchemaVersion))
		}
		if !known[j.CaseID] {
			problems = append(problems, fmt.Errorf("%s: %w %q", where, ErrUnknownCase, j.CaseID))
		}
		if !j.Relevance.Valid() {
			problems = append(problems, fmt.Errorf("%s: %w %d", where, ErrUnknownGrade, int(j.Relevance)))
		}
		if err := checkRoot(j.LineageRoot); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", where, err))
		}
		if j.Required && j.Forbidden {
			problems = append(problems, fmt.Errorf("%s: %w", where, ErrContradictoryJudgment))
		}

		key := j.CaseID + "\x00" + string(j.LineageRoot)
		if seen[key] {
			problems = append(problems, fmt.Errorf("%s: %w", where, ErrDuplicateJudgment))
		}
		seen[key] = true
	}

	return errors.Join(problems...)
}

// checkAssertions checks every lineage root a case names. An assertion keyed
// on an unparseable root can never match a candidate, so it would pass
// vacuously forever.
func checkAssertions(c Case) []error {
	problems := checkExpectedFail(c)
	if c.Assertions == nil {
		return problems
	}
	report := func(field string, root recall.LineageRoot) {
		if err := checkRoot(root); err != nil {
			problems = append(problems, fmt.Errorf("case %q: assertions.%s: %w", c.CaseID, field, err))
		}
	}
	for root := range c.Assertions.ExpectedRevisions {
		report("expected_revisions", root)
	}
	for _, root := range c.Assertions.SuppressedLineages {
		report("suppressed_lineages", root)
	}
	for root := range c.Assertions.WithheldLineages {
		report("withheld_lineages", root)
	}
	for _, root := range c.Assertions.VisibleLineages {
		report("visible_lineages", root)
	}
	if root := c.Assertions.ExpectedTopLineage; root != "" {
		report("expected_top_lineage", root)
	}
	for root := range c.Assertions.ExcerptContains {
		report("excerpt_contains", root)
	}
	if min, max := c.Assertions.MinResults, c.Assertions.MaxResults; min != nil && max != nil && *min > *max {
		problems = append(problems, fmt.Errorf("case %q: %w: %d > %d",
			c.CaseID, ErrImpossibleResultBounds, *min, *max))
	}
	return problems
}

// checkExpectedFail confirms every excused assertion is one that exists and
// one this case states.
//
// Both halves matter for the same reason. A name outside the vocabulary
// excuses nothing and reports itself as fixed on the first run; a real name
// the case never declared can never be violated, so it reports itself as fixed
// forever. Either way the marking says a defect is being watched when nothing
// is watching it.
func checkExpectedFail(c Case) []error {
	if c.ExpectedFail == nil {
		return nil
	}
	known := make(map[string]bool, len(AssertionFields))
	for _, name := range AssertionFields {
		known[name] = true
	}
	declared := c.Assertions.Declared()

	var problems []error
	for _, name := range c.ExpectedFail.Assertions {
		switch {
		case !known[name]:
			problems = append(problems, fmt.Errorf("case %q: expected_fail: %w %q",
				c.CaseID, ErrUnknownAssertionName, name))
		case !declared[name]:
			problems = append(problems, fmt.Errorf("case %q: %w: %q",
				c.CaseID, ErrVacuousExpectedFail, name))
		}
	}
	return problems
}

// checkRoot confirms a lineage root is a persisted-form locator. Parsing is
// delegated so the pack format cannot drift from the locator format.
func checkRoot(root recall.LineageRoot) error {
	loc, err := root.Locator()
	if err != nil {
		return fmt.Errorf("%w %q: %w", ErrMalformedLineageRoot, string(root), err)
	}
	if loc.SourceUID == "" || loc.Local == "" {
		return fmt.Errorf("%w %q: want <source_uid>:<local>", ErrMalformedLineageRoot, string(root))
	}
	return nil
}
