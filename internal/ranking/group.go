package ranking

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/recall"
)

// group is one lineage root and every candidate that projects it. Two
// candidates in one group are one record seen twice, never two pieces of
// evidence.
type group struct {
	root       recall.LineageRoot
	lineages   []lineage.Lineage
	candidates []recall.Candidate

	// bestRank is the best (lowest) local rank this record reached in each
	// source. A source's later hits on the same record add nothing.
	bestRank map[recall.SourceUID]int

	// bestRelevance is the relevance of the candidate that earned bestRank, per
	// source — not the best relevance seen, which could belong to a different
	// candidate and a different rank.
	bestRelevance map[recall.SourceUID]float64

	// score is lineage_score(g): the maximum over sources, not the sum.
	score float64

	primary recall.Candidate

	// explain is built while the group is scored, so it carries the exact prior
	// and rank constant the arithmetic used. The cluster stage fills the fields
	// only a cluster knows.
	explain recall.Explanation

	// exact is set when any candidate in the group matched an identifier
	// exactly. Promotion partitions on it rather than adding to score.
	exact bool

	// exactAlias is set only when the SAME candidate carries exact_identifier
	// and alias. Two members of one lineage group cannot assemble the stronger
	// signal between them: candidate-specific evidence must stay specific.
	exactAlias bool

	// exactIdentity is likewise candidate-specific: the candidate carries
	// exact_identifier and one of its existing identity fields names a stable
	// identifier token from this request.
	exactIdentity bool
}

// groupByLineage performs step 3 of the ranking pipeline: it resolves each
// candidate's lineage root, collects the candidates sharing one, and scores the
// group.
func (r *Ranker) groupByLineage(req Request) ([]*group, error) {
	graph := lineage.NewGraph(req.Resolver, req.Candidates)
	for _, from := range sortedUIDs(req.SourceDerivations) {
		graph.DeclareSourceDerivation(from, req.SourceDerivations[from])
	}

	// The local pool size is the list the rank came from: an explanation that
	// says "rank 3" without saying "of 3" reads as a weak hit when it was the
	// source's whole answer.
	pool := make(map[recall.SourceUID]int, len(req.Candidates))
	for _, c := range req.Candidates {
		pool[c.SourceUID]++
	}

	byRoot := make(map[recall.LineageRoot]*group, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.LocalRank < 1 {
			return nil, fmt.Errorf("%w: %s has local_rank %d, want one-based",
				ErrCandidate, c.Locator, c.LocalRank)
		}
		lin, err := graph.Of(c)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrCandidate, c.Locator, err)
		}

		g := byRoot[lin.Root]
		if g == nil {
			g = &group{
				root:          lin.Root,
				bestRank:      make(map[recall.SourceUID]int, 2),
				bestRelevance: make(map[recall.SourceUID]float64, 2),
			}
			byRoot[lin.Root] = g
		}
		g.lineages = append(g.lineages, lin)
		g.candidates = append(g.candidates, c)
		// Relevance travels with the rank it belongs to. Taking the best of each
		// separately would score a group with one candidate's rank and another's
		// relevance, which is an arithmetic no candidate supports.
		if best, seen := g.bestRank[c.SourceUID]; !seen || c.LocalRank < best {
			g.bestRank[c.SourceUID] = c.LocalRank
			g.bestRelevance[c.SourceUID] = relevanceOf(c)
		}
		g.exact = g.exact || c.Exact()
		g.exactAlias = g.exactAlias ||
			(c.Exact() && c.HasSignal(recall.MatchAlias))
		g.exactIdentity = g.exactIdentity ||
			(c.Exact() && candidateNamesStableIdentifier(c, req.StableIdentifiers))
	}

	groups := make([]*group, 0, len(byRoot))
	for _, g := range byRoot {
		slices.SortFunc(g.candidates, byLocalOrder)
		groups = append(groups, g)
	}
	// Root order is the one ordering available before scores exist, so later
	// stages inherit a deterministic starting point regardless of adapter
	// return order.
	slices.SortFunc(groups, func(a, b *group) int { return cmp.Compare(a.root, b.root) })

	for _, g := range groups {
		if err := r.scoreGroup(g, req.QueryClass, pool); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

func candidateNamesStableIdentifier(c recall.Candidate, identifiers []string) bool {
	identities := []string{c.CandidateID, c.SourceRecordID, c.Locator.Local, c.Title}
	// These keys are identity-bearing across structured and document adapters.
	// Do not scan arbitrary metadata: an excerpt, note, or description naming a
	// td id is content, not evidence that this exact candidate IS that id.
	for _, key := range []string{"name", "path", "relative_path"} {
		if identity, ok := c.Metadata[key].(string); ok {
			identities = append(identities, identity)
		}
	}
	for _, identity := range identities {
		identity = strings.ToLower(strings.TrimSpace(identity))
		if identity == "" {
			continue
		}
		identity = strings.SplitN(identity, "#", 2)[0]
		base := identity
		if at := strings.LastIndexAny(base, `/\`); at >= 0 {
			base = base[at+1:]
		}
		for _, identifier := range identifiers {
			if identifier == identity || identifier == base {
				return true
			}
		}
	}
	return false
}

// scoreGroup computes lineage_score and builds the part of the explanation that
// only the group knows. Score and explanation are produced together: there is
// no second pass that could recompute either one differently.
//
//	lineage_score(g) = max over sources s of
//	                       prior(s) * relevance(s) / (rank_constant + best_rank(s))
//
// Maximum, not sum: one record seen from two sources is still one record.
//
// Relevance is a FACTOR rather than a term because it scales a belief rather
// than competing with one: a source trusted twice as much and matched half as
// well lands in the same place. Why fusion needs it at all is argued once, in
// this package's doc comment; what it defends against concretely is a 7% prior
// edge buying five rank positions at rank_constant 60.
func (r *Ranker) scoreGroup(g *group, class string, pool map[recall.SourceUID]int) error {
	for _, uid := range sortedRanks(g.bestRank) {
		p, err := r.cfg.prior(uid, class)
		if err != nil {
			return err
		}
		s := p.Effective * g.bestRelevance[uid] / (r.cfg.RankConstant + float64(g.bestRank[uid]))
		if s > g.score {
			g.score = s
		}
	}

	// The primary is the candidate a person is shown: highest source prior,
	// then the best rank that prior earned, then locator order so the choice
	// never depends on which adapter answered first.
	var bestPrior recall.PriorExplanation
	for i, c := range g.candidates {
		p, err := r.cfg.prior(c.SourceUID, class)
		if err != nil {
			return err
		}
		if i == 0 || p.Effective > bestPrior.Effective {
			g.primary, bestPrior = c, p
		}
	}

	g.explain = recall.Explanation{
		SourceUID:     g.primary.SourceUID,
		SourceID:      r.cfg.Sources[g.primary.SourceUID].SourceID,
		LocalRank:     g.primary.LocalRank,
		LocalPoolSize: pool[g.primary.SourceUID],
		MatchSignals:  slices.Clone(g.primary.MatchSignals),
		Prior:         bestPrior,
		Relevance:     g.primary.Relevance,
		LineageRoot:   g.root,
		RankConstant:  r.cfg.RankConstant,
		Freshness: recall.FreshnessExplanation{
			SourceRevision: g.primary.SourceRevision,
			ObservedAt:     g.primary.ObservedAt,
			ConfirmedAt:    g.primary.ConfirmedAt,
		},
	}
	return nil
}

// byLocalOrder is a total order over candidates: best local rank first, then
// locator, then candidate identity. It ends in a unique key so no two
// candidates ever compare equal and sorting cannot depend on input order.
func byLocalOrder(a, b recall.Candidate) int {
	return cmp.Or(
		cmp.Compare(a.LocalRank, b.LocalRank),
		cmp.Compare(a.Locator.String(), b.Locator.String()),
		cmp.Compare(a.CandidateID, b.CandidateID),
	)
}

func sortedRanks(m map[recall.SourceUID]int) []recall.SourceUID {
	out := make([]recall.SourceUID, 0, len(m))
	for uid := range m {
		out = append(out, uid)
	}
	slices.Sort(out)
	return out
}

func sortedUIDs(m map[recall.SourceUID]recall.SourceUID) []recall.SourceUID {
	out := make([]recall.SourceUID, 0, len(m))
	for uid := range m {
		out = append(out, uid)
	}
	slices.Sort(out)
	return out
}

// relevanceOf reads a candidate's relevance, defaulting to 1.0.
//
// The default is what lets a source that predates this factor — or an
// out-of-tree adapter that has not been rebuilt — keep the ordering it had.
// Values outside [0,1] are clamped rather than rejected: a malformed number
// from one source should cost that source its position, not fail the query.
func relevanceOf(c recall.Candidate) float64 {
	if c.Relevance == nil {
		return 1
	}
	switch v := *c.Relevance; {
	case math.IsNaN(v), v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
