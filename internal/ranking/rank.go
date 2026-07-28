package ranking

import (
	"errors"
	"fmt"
	"slices"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/recall"
)

// ErrRequest reports a fusion request that cannot be served, such as one with
// no way to resolve a source name to its immutable identity.
var ErrRequest = errors.New("invalid fusion request")

// Request is one fusion input. Everything ranking needs arrives here: it reads
// no configuration file, no clock, and no source.
type Request struct {
	// Candidates is the pooled result of every source that answered, in any
	// order. Ordering the pool is this package's job, so the caller is free to
	// append results as adapters return them.
	Candidates []recall.Candidate

	// Resolver maps source display names to immutable identities, for the
	// derived_from edges candidates carry.
	Resolver lineage.Resolver

	// SourceDerivations are manifest-declared whole-source projections, keyed by
	// the projecting source. They suppress corroboration without changing any
	// lineage root: a source-level edge cannot say which record it projects.
	SourceDerivations map[recall.SourceUID]recall.SourceUID

	// Limit caps the results emitted, overriding [Config.Limit]. Zero means the
	// configured default applies.
	Limit int

	// QueryClass names the request's query class, selecting at most one intent
	// prior per source and governing intent-sensitive ranking rules. Empty
	// means no intent rule fires and exact identifiers retain their legacy
	// lookup behavior.
	QueryClass string

	// StableIdentifiers are the normalized identifier-shaped tokens the query
	// named. In a multi-token identifier query, only an exact candidate whose
	// own identity matches one of these may partition; a stable td id elsewhere
	// in a sentence must not promote an unrelated project-name exact hit.
	StableIdentifiers []string

	// Mode distinguishes a user-visible request from a host's pre-reply budget.
	// SuppressLineages applies only to pre-reply: suppression filters passive
	// display and never hides evidence someone asked for.
	Mode recall.InvocationMode

	// SuppressLineages are roots the host has already shown.
	SuppressLineages []recall.LineageRoot
}

// Fusion is the ordered result set plus an account of everything that did not
// reach it. A withheld candidate is always counted with a reason; nothing
// disappears silently.
type Fusion struct {
	Results    []recall.Result
	Suppressed []recall.Suppression

	// Truncated means the limit dropped trailing results. It is not degraded
	// coverage: every source that answered still contributed to the ordering.
	Truncated bool
	Dropped   int

	// Limit is the result budget that was in force: the request's when it named
	// one, the configured default otherwise, zero when unbounded. It is reported
	// so a response can state the rule that decided its length instead of
	// leaving a caller to infer it from the count — and a caller who cannot tell
	// a short answer from a truncated one cannot tell a corpus with two answers
	// from a budget with room for two.
	Limit int
}

// Ranker fuses candidate pools under one validated configuration. It holds no
// per-request state, so one Ranker serves concurrent requests.
type Ranker struct{ cfg Config }

// New validates a configuration and returns a Ranker that applies it. An
// out-of-range prior fails here rather than being clamped: clamping would let a
// machine rank differently from the configuration that was reviewed.
func New(cfg Config) (*Ranker, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Ranker{cfg: cfg}, nil
}

// Config returns the configuration in force, with defaults resolved. The caller
// reports it in the retrieval plan, so what ran and what is reported cannot
// diverge.
func (r *Ranker) Config() Config { return r.cfg }

// Fuse runs the whole pipeline: lineage grouping, clustering and corroboration,
// exact-match promotion, and diversity selection.
//
// Explanations are built as the scores are computed, not recomputed afterwards,
// so a result's explanation is the arithmetic that produced it.
func (r *Ranker) Fuse(req Request) (Fusion, error) {
	limit := r.limit(req)
	if len(req.Candidates) == 0 {
		return Fusion{Limit: limit}, nil
	}
	if req.Resolver == nil {
		return Fusion{}, fmt.Errorf("%w: no resolver; lineage edges name sources", ErrRequest)
	}

	groups, err := r.groupByLineage(req)
	if err != nil {
		return Fusion{}, err
	}
	clusters := r.clusterGroups(groups)
	promote(clusters, req)
	return r.selectResults(clusters, req), nil
}

// limit resolves the result budget in force for one request.
//
// A per-request limit overrides the configured one. The configured value is a
// policy default; the request's is what this caller asked for, and a long-lived
// service serving many callers from one Ranker cannot vary it otherwise.
func (r *Ranker) limit(req Request) int {
	if req.Limit > 0 {
		return req.Limit
	}
	return r.cfg.Limit
}

// promote performs step 5. A cluster containing an exact identifier match sorts
// above every cluster without one for an identifier-shaped or unclassified
// request, ordered among themselves by cluster score. Natural-language
// requests keep the adapter's exact signal and its scored relevance, but do
// not interpret a named subject inside a sentence as an identifier lookup. A
// declared alias remains promotable because MatchAlias is candidate-specific
// evidence that this exact candidate, rather than some other word in the
// query, was named.
//
// The active promotion remains a partition, not a bonus: no amount of
// corroboration lets a lexical cluster overtake an exact hit, and no exact hit
// gets an unexplainable number added to its score.
func promote(clusters []*cluster, req Request) {
	for _, c := range clusters {
		c.explain.ExactPromoted = promotable(c, req)
	}
	slices.SortFunc(clusters, func(a, b *cluster) int {
		aExact := promotable(a, req)
		bExact := promotable(b, req)
		if aExact != bExact {
			if aExact {
				return -1
			}
			return 1
		}
		return compareRelevance(a, b)
	})
}

func promotable(c *cluster, req Request) bool {
	if !c.exact {
		return false
	}
	if c.exactAlias {
		return true
	}
	switch req.QueryClass {
	case QueryClassNaturalLanguage:
		return false
	case QueryClassIdentifier:
		// A plain one-token name has no stable syntax to correlate. Preserve
		// its original exact behavior. Stable syntax, including when wrapped
		// in prose, must name this candidate specifically.
		return len(req.StableIdentifiers) == 0 || c.exactIdentity
	default:
		return true
	}
}
