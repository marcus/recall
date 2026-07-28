package ranking

import (
	"cmp"
	"slices"

	"github.com/marcus/recall/internal/recall"
)

// compareRelevance orders two clusters within one exactness partition: higher
// score first, then the score-basis candidate's locator and identity. The
// display representative is deliberately absent: choosing a more useful chunk
// to show must not become a hidden reranker. The tail of the comparison is a
// unique key, so no two clusters ever compare equal and the order cannot depend
// on which adapter answered first.
func compareRelevance(a, b *cluster) int {
	return cmp.Or(
		cmp.Compare(b.score, a.score),
		cmp.Compare(a.scoreBasis.Locator.String(), b.scoreBasis.Locator.String()),
		cmp.Compare(a.scoreBasis.CandidateID, b.scoreBasis.CandidateID),
		cmp.Compare(a.explain.LineageRoot, b.explain.LineageRoot),
	)
}

// selectResults performs step 7: diversity selection after relevance.
//
// Relevance decides the order; selection decides what is shown. It withholds
// evidence the host says it has already shown, withholds near-duplicates that
// clustering deliberately refused to merge, and demotes a source that would
// otherwise fill the whole answer. Every withheld cluster is counted with a
// reason.
func (r *Ranker) selectResults(ordered []*cluster, req Request) Fusion {
	var out Fusion

	suppress := make(map[recall.LineageRoot]bool, len(req.SuppressLineages))
	// Suppression filters passive display only. An explicit request is someone
	// asking, and the answer never hides what they asked for.
	if req.Mode == recall.ModePreReply {
		for _, root := range req.SuppressLineages {
			suppress[root] = true
		}
	}

	kept, withheld := r.withhold(ordered, suppress, r.cfg.RelevanceFloor)
	if len(kept) == 0 && r.cfg.RelevanceFloor > 0 {
		// A floor may withhold; it may not abstain. An empty answer is read
		// everywhere as a claim about the corpus: the CLI exits 2, the MCP text
		// tells a model that reporting nothing was found is supported, and the
		// live profile's `must_abstain` check rests on the asymmetry that adding
		// a source can turn "nothing" into "something" and nothing can turn it
		// back. A configured threshold may not turn it back with the corpus
		// unchanged.
		//
		// The test is run against what SURVIVED the other rules, not against the
		// whole cluster list, because those rules compose: a floor that kept its
		// one weak result while lineage suppression removed the strong one still
		// produced the empty answer, one rule each. Re-running the pass is what
		// makes the question "is this answer empty because of the floor", which
		// is the question the rule is about. It costs a second pass over a list
		// that is already known to be empty.
		//
		// The exemption makes the rule non-monotonic, and that is the intended
		// shape rather than an oversight: raising a floor can return MORE
		// results, because a floor high enough to catch everything catches
		// nothing. Deleting the last record above a fixed floor does the same to
		// every record below it. Both read as a bug from outside, and the
		// alternative is a configured number that can assert absence.
		kept, withheld = r.withhold(ordered, suppress, 0)
	}
	out.Suppressed = withheld

	final, demoted := r.diversify(kept)

	limit := r.limit(req)
	out.Limit = limit
	if limit > 0 && len(final) > limit {
		for _, c := range final[limit:] {
			// A cluster that would have fitted on relevance alone and lost its
			// place to the diversity policy is suppressed by that policy, not
			// truncated by the budget. The distinction is what makes the policy
			// reviewable.
			if demoted[c] {
				out.suppress(recall.SuppressDiversity, c)
				continue
			}
			out.Dropped++
		}
		out.Truncated = out.Dropped > 0
		final = final[:limit]
	}

	out.Results = make([]recall.Result, 0, len(final))
	for _, c := range final {
		// Reported only for a cluster that is shown. A view of a record the
		// caller never received is already accounted for by whatever withheld
		// the cluster, and naming it twice would overstate what was held back.
		for _, class := range c.viewClasses {
			for _, g := range class[1:] {
				out.Suppressed = append(out.Suppressed, recall.Suppression{
					Reason: recall.SuppressDuplicateView,
					// One slot, not one candidate. The group's candidates were
					// already one record seen more than once and would never
					// have been results of their own, so the answer is exactly
					// one result shorter for each view folded into another.
					Count:       1,
					LineageRoot: g.root,
					FusedInto:   c.explain.LineageRoot,
				})
			}
		}
		out.Results = append(out.Results, c.result())
	}
	slices.SortFunc(out.Suppressed, func(a, b recall.Suppression) int {
		return cmp.Or(
			cmp.Compare(a.Reason, b.Reason),
			cmp.Compare(a.LineageRoot, b.LineageRoot),
		)
	})
	return out
}

// withhold runs the display rules over the ordered clusters: what the host has
// already seen, what would read as the same text twice, and what nothing in is
// about the query. It returns what survives and an account of what did not.
//
// floor is passed rather than read from the configuration so the caller can run
// the pass again without it. Nothing else here varies between the two runs, and
// the second one starts from an empty `shown` map for the same reason the first
// does: no cluster survived to claim a fingerprint.
func (r *Ranker) withhold(
	ordered []*cluster,
	suppress map[recall.LineageRoot]bool,
	floor float64,
) (kept []*cluster, withheld []recall.Suppression) {
	hold := func(reason string, c *cluster) {
		withheld = append(withheld, suppression(reason, c))
	}

	kept = make([]*cluster, 0, len(ordered))
	shown := make(map[string]bool, len(ordered))
	for _, c := range ordered {
		// The floor is checked before the fingerprint is claimed, so a cluster
		// nobody should be shown never withholds an honest one as its duplicate.
		if floor > 0 && r.irrelevant(c, floor) {
			hold(recall.SuppressRelevanceFloor, c)
			continue
		}
		if len(suppress) > 0 && allSuppressed(c, suppress) {
			hold(recall.SuppressLineageSeen, c)
			continue
		}
		// A near-duplicate here is a cluster that clustering refused to merge —
		// a different record type carrying the same content, say — but which a
		// reader would see as the same text twice.
		prints := fingerprints(c)
		if anyShown(prints, shown) {
			hold(recall.SuppressDuplicate, c)
			continue
		}
		for _, fp := range prints {
			shown[fp] = true
		}
		kept = append(kept, c)
	}
	return kept, withheld
}

// irrelevant reports whether nothing in a cluster is about the query, on the
// one definition of relevance every source computes the same way.
//
// It is a selection policy and it belongs here, beside the other rules that
// decide what is shown rather than what is true. Withholding the CANDIDATES
// before grouping was tried first and is wrong in three ways that all have the
// same shape: a record the caller will not see still carries information about
// the records they will. Its view of a record is what tells host suppression
// that the record was already shown; its sibling chunk is what
// [representativeGroup] compares against when choosing which text to display;
// its lineage is what a derivation chain walks through. Removing it early
// re-showed a suppressed record, degraded an excerpt to a bare heading, and
// shortened a chain — none of which is a thing a floor is for.
//
// So the unit is the cluster, and the test is "nothing in it", not "its best
// record". A cluster survives if any record in it clears the floor, because a
// cluster is one thing the caller is shown and the floor is a claim about that
// thing.
//
// An exact identifier match is exempt outright: a record named by name need not
// describe itself. internal/adapters/docs states the same rule for its own
// coverage floor, and smoke's exact-task-id case is an exact hit whose text
// carries no query term at all, at relevance 0.
//
// The exemption is unconditional where [promotable] is not, and the asymmetry
// is deliberate: promotion decides whether an exact hit outranks everything,
// which a project name inside a sentence has not earned, while this decides
// whether it is shown at all, which it has. A caller who typed the name gets
// the record either way.
func (r *Ranker) irrelevant(c *cluster, floor float64) bool {
	if c.exact {
		return false
	}
	for _, g := range c.groups {
		for _, cand := range g.candidates {
			if floorRelevance(cand) >= floor {
				return false
			}
		}
	}
	return true
}

// floorRelevance is the number the relevance floor judges a candidate by.
//
// It differs from [relevanceOf] in exactly one case, deliberately. relevanceOf
// clamps a malformed number to 0, so a source that sends one loses its
// position — which is the right cost when the number only orders things. A
// floor would turn that lost position into the source's disappearance from
// every answer on the machine, which is a much larger penalty for the same
// mistake, and one an adapter computing matched/total with total = 0 would earn
// silently. A number that is not a number is not a claim, so it reads here the
// way an absent one does: the source asserted nothing, and a rule expressed in
// what it did not assert may not withhold it.
func floorRelevance(c recall.Candidate) float64 {
	if c.Relevance == nil || !finite(*c.Relevance) {
		return 1
	}
	return relevanceOf(c)
}

// diversify moves a source's surplus clusters below other sources' clusters. It
// is a selection policy applied after relevance, never a substitute for it:
// nothing is reordered within a source, and a demoted cluster is still emitted
// when there is room for it.
func (r *Ranker) diversify(kept []*cluster) (final []*cluster, demoted map[*cluster]bool) {
	demoted = make(map[*cluster]bool)
	if r.cfg.MaxPerSource <= 0 {
		return kept, demoted
	}

	seen := make(map[recall.SourceUID]int, len(kept))
	head := make([]*cluster, 0, len(kept))
	tail := make([]*cluster, 0, len(kept))
	for _, c := range kept {
		uid := c.primary.SourceUID
		if seen[uid] >= r.cfg.MaxPerSource {
			demoted[c] = true
			tail = append(tail, c)
			continue
		}
		seen[uid]++
		head = append(head, c)
	}
	return append(head, tail...), demoted
}

// allSuppressed reports whether every record in a cluster is one the host has
// already shown. A cluster holding evidence the host has not seen is still
// worth showing, so suppression is all-or-nothing per cluster.
//
// Records, not roots: two views of one record are one thing, so showing either
// one has shown it. Read root by root instead, a cluster whose displayed view
// the host suppressed would keep its place on the strength of the other view's
// root and re-display the record under the very view that was suppressed.
func allSuppressed(c *cluster, suppress map[recall.LineageRoot]bool) bool {
	for _, class := range c.viewClasses {
		if !anySuppressed(class, suppress) {
			return false
		}
	}
	return true
}

func anySuppressed(class []*group, suppress map[recall.LineageRoot]bool) bool {
	for _, g := range class {
		if suppress[g.root] {
			return true
		}
	}
	return false
}

// fingerprints keys near-duplicate suppression, scoped by source.
//
// Unscoped, this was a second and cheaper lever for one source to remove
// another's evidence from a response: echo the fingerprint, score high enough
// to be considered first, and the honest cluster is silently suppressed as a
// near-duplicate. Suppression is a display policy, so it may only ever hide a
// source's repetition of itself.
func fingerprints(c *cluster) []string {
	var out []string
	for _, g := range c.groups {
		for _, cand := range g.candidates {
			if cand.ContentFingerprint != "" && cand.SourceUID != "" {
				out = append(out, key("fp", string(cand.SourceUID), cand.ContentFingerprint))
			}
		}
	}
	return dedupe(out)
}

func anyShown(prints []string, shown map[string]bool) bool {
	for _, fp := range prints {
		if shown[fp] {
			return true
		}
	}
	return false
}

// suppress records a withheld cluster.
func (f *Fusion) suppress(reason string, c *cluster) {
	f.Suppressed = append(f.Suppressed, suppression(reason, c))
}

// suppression is one withheld cluster. Count is the number of candidates the
// host is not being shown, so it can say what it is withholding without being
// told what the records were.
func suppression(reason string, c *cluster) recall.Suppression {
	n := 0
	for _, g := range c.groups {
		n += len(g.candidates)
	}
	return recall.Suppression{
		Reason:      reason,
		Count:       n,
		LineageRoot: c.explain.LineageRoot,
	}
}

// result renders a cluster for the response. Members keep one entry per lineage
// group: two candidates inside one member mean one record seen twice, and two
// members mean two roots — two records, unless one of them is reported as a
// duplicate view of the other. What is independent evidence is the
// corroboration count, never the member count.
func (c *cluster) result() recall.Result {
	members := make([]recall.ClusterMember, 0, len(c.groups))
	for _, g := range c.groups {
		members = append(members, recall.ClusterMember{
			LineageRoot: g.root,
			Candidates:  slices.Clone(g.candidates),
		})
	}
	slices.SortFunc(members, func(a, b recall.ClusterMember) int {
		return cmp.Compare(a.LineageRoot, b.LineageRoot)
	})
	return recall.Result{
		Primary:     c.primary,
		Members:     members,
		Explanation: c.explain,
		Score:       c.score,
	}
}
