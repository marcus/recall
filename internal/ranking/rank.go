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

	// QueryClass names the request's query class, selecting at most one intent
	// prior per source. Empty means no intent rule fires and base priors apply.
	QueryClass string

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
	if len(req.Candidates) == 0 {
		return Fusion{}, nil
	}
	if req.Resolver == nil {
		return Fusion{}, fmt.Errorf("%w: no resolver; lineage edges name sources", ErrRequest)
	}

	groups, err := r.groupByLineage(req)
	if err != nil {
		return Fusion{}, err
	}
	clusters := r.clusterGroups(groups)
	promote(clusters)
	return r.selectResults(clusters, req), nil
}

// promote performs step 5. A cluster containing an exact identifier match sorts
// above every cluster without one, ordered among themselves by cluster score.
// It is a partition, not a bonus: no amount of corroboration lets a lexical
// cluster overtake an exact hit, and no exact hit gets an unexplainable number
// added to its score.
func promote(clusters []*cluster) {
	slices.SortFunc(clusters, func(a, b *cluster) int {
		if a.exact != b.exact {
			if a.exact {
				return -1
			}
			return 1
		}
		return compareRelevance(a, b)
	})
}
