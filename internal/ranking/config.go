package ranking

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/marcus/recall/internal/recall"
)

// Fusion defaults. These are evaluation parameters, not tuning knobs: a change
// here is a change the benchmark must justify.
const (
	// DefaultRankConstant damps the difference between adjacent local ranks so
	// one source's second hit is not dwarfed by another's first.
	DefaultRankConstant = 60.0

	// DefaultCorroborationCap bounds how much independent agreement can add to a
	// cluster, in multiples of its strongest lineage group.
	DefaultCorroborationCap = 2.0

	// MinPrior and MaxPrior bound a source's authority. The range is deliberately
	// narrow: a prior expresses expected authority for a query class, and
	// configuration must not become an unbounded scoring language.
	MinPrior = 0.5
	MaxPrior = 2.0
)

// ErrConfig reports a configuration defect. A prior outside its range is not
// clamped: silently correcting it would hide the defect and make two machines
// with the same configuration rank differently from the one that was validated.
var ErrConfig = errors.New("invalid ranking configuration")

// ErrCandidate reports a candidate that cannot be fused, such as one carrying
// no local rank. Fusion fails rather than inventing a rank, because a missing
// rank would silently score as if it were better than rank 1.
var ErrCandidate = errors.New("invalid candidate")

// Config is everything configuration contributes to fusion. It is the complete
// list: if a value is not here, it did not affect an ordering, and if it is
// here it appears in every explanation it touched.
type Config struct {
	// RankConstant is the k in prior/(k+rank). Zero means [DefaultRankConstant].
	RankConstant float64

	// CorroborationCap bounds a cluster score at this multiple of its strongest
	// lineage group. Zero means [DefaultCorroborationCap].
	CorroborationCap float64

	// Sources is the per-source prior configuration, keyed by immutable
	// identity. A candidate from a source that is absent here is a defect in the
	// caller's plan, not something fusion guesses a prior for.
	Sources map[recall.SourceUID]SourceConfig

	// Limit is the maximum number of results emitted. Zero means unlimited.
	Limit int

	// MaxPerSource is how many results one source may contribute before its
	// remaining clusters are demoted below other sources' clusters. Zero
	// disables the diversity policy. Diversity is applied after relevance and
	// never reorders within a source.
	MaxPerSource int
}

// SourceConfig is one source instance's ranking configuration. It is the whole
// of what ranking knows about a source: expected authority for a query class,
// and nothing about how the source retrieves or what it stores.
type SourceConfig struct {
	// SourceID is the display name, carried so an explanation can name the
	// source without ranking acquiring a resolver for it.
	SourceID string

	// BasePrior is the cross-source prior, in [MinPrior, MaxPrior].
	BasePrior float64

	// IntentPriors are bounded adjustments per named query class. At most one
	// applies to a request; a duplicated class is rejected rather than resolved
	// by declaration order.
	IntentPriors []IntentPrior
}

// IntentPrior is a bounded adjustment to a base prior for one named query
// class.
type IntentPrior struct {
	// Rule names the configured rule, and is reported in the explanation. An
	// adjustment that cannot be attributed to a rule is not applied.
	Rule string

	// QueryClass is the request's declared class this rule answers to.
	QueryClass string

	// Adjustment is added to the base prior. The sum must stay in
	// [MinPrior, MaxPrior].
	Adjustment float64
}

// withDefaults fills unset constants and copies the source map so a later
// mutation by the caller cannot change how a request already in flight ranks.
func (c Config) withDefaults() Config {
	if c.RankConstant == 0 {
		c.RankConstant = DefaultRankConstant
	}
	if c.CorroborationCap == 0 {
		c.CorroborationCap = DefaultCorroborationCap
	}
	sources := make(map[recall.SourceUID]SourceConfig, len(c.Sources))
	for uid, sc := range c.Sources {
		sc.IntentPriors = slices.Clone(sc.IntentPriors)
		sources[uid] = sc
	}
	c.Sources = sources
	return c
}

// Validate reports configuration defects. It is called by [New]; it is exported
// because `recall doctor` must be able to fail on the same conditions before a
// query is ever run.
func (c Config) Validate() error {
	if !finite(c.RankConstant) || c.RankConstant <= 0 {
		return fmt.Errorf("%w: rank_constant = %v, want a positive finite number",
			ErrConfig, c.RankConstant)
	}
	// A cap below 1 would make corroborating evidence score *less* than the
	// single best group, which inverts the meaning of the parameter.
	if !finite(c.CorroborationCap) || c.CorroborationCap < 1 {
		return fmt.Errorf("%w: corroboration_cap = %v, want >= 1",
			ErrConfig, c.CorroborationCap)
	}
	if c.Limit < 0 {
		return fmt.Errorf("%w: limit = %d, want >= 0", ErrConfig, c.Limit)
	}
	if c.MaxPerSource < 0 {
		return fmt.Errorf("%w: max_per_source = %d, want >= 0", ErrConfig, c.MaxPerSource)
	}

	for _, uid := range sortedKeys(c.Sources) {
		if err := c.Sources[uid].validate(uid); err != nil {
			return err
		}
	}
	return nil
}

func (sc SourceConfig) validate(uid recall.SourceUID) error {
	if !inPriorRange(sc.BasePrior) {
		return fmt.Errorf("%w: source %q base_prior = %v, want [%v, %v]",
			ErrConfig, uid, sc.BasePrior, MinPrior, MaxPrior)
	}
	seen := make(map[string]bool, len(sc.IntentPriors))
	for _, ip := range sc.IntentPriors {
		switch {
		case ip.QueryClass == "":
			return fmt.Errorf("%w: source %q has an intent prior with no query class",
				ErrConfig, uid)
		case ip.Rule == "":
			// An unattributable adjustment cannot be explained, and a value that
			// cannot appear in an explanation does not exist.
			return fmt.Errorf("%w: source %q intent prior for %q names no rule",
				ErrConfig, uid, ip.QueryClass)
		case seen[ip.QueryClass]:
			return fmt.Errorf("%w: source %q has two intent priors for query class %q",
				ErrConfig, uid, ip.QueryClass)
		case !inPriorRange(sc.BasePrior + ip.Adjustment):
			return fmt.Errorf("%w: source %q rule %q gives effective prior %v, want [%v, %v]",
				ErrConfig, uid, ip.Rule, sc.BasePrior+ip.Adjustment, MinPrior, MaxPrior)
		}
		seen[ip.QueryClass] = true
	}
	return nil
}

// prior resolves the effective prior for a source under a query class, together
// with the components an explanation must show. It is the only place a prior is
// read, so no result can carry a prior that was not explained.
func (c Config) prior(uid recall.SourceUID, class string) (recall.PriorExplanation, error) {
	sc, ok := c.Sources[uid]
	if !ok {
		return recall.PriorExplanation{}, fmt.Errorf(
			"%w: no prior configured for source %q", ErrConfig, uid)
	}
	p := recall.PriorExplanation{Base: sc.BasePrior, Effective: sc.BasePrior}
	if class == "" {
		return p, nil
	}
	for _, ip := range sc.IntentPriors {
		if ip.QueryClass != class {
			continue
		}
		p.Intent = ip.Adjustment
		p.Rule = ip.Rule
		p.Effective = sc.BasePrior + ip.Adjustment
		break
	}
	return p, nil
}

func inPriorRange(v float64) bool { return finite(v) && v >= MinPrior && v <= MaxPrior }

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func sortedKeys(m map[recall.SourceUID]SourceConfig) []recall.SourceUID {
	out := make([]recall.SourceUID, 0, len(m))
	for uid := range m {
		out = append(out, uid)
	}
	slices.Sort(out)
	return out
}
