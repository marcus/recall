package ranking

import (
	"cmp"
	"slices"

	"github.com/marcus/recall/internal/recall"
)

// compareRelevance orders two clusters within one exactness partition: higher
// score first, then the primary's locator and identity. The tail of the
// comparison is a unique key, so no two clusters ever compare equal and the
// order cannot depend on which adapter answered first.
func compareRelevance(a, b *cluster) int {
	return cmp.Or(
		cmp.Compare(b.score, a.score),
		cmp.Compare(a.primary.Locator.String(), b.primary.Locator.String()),
		cmp.Compare(a.primary.CandidateID, b.primary.CandidateID),
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

	kept := make([]*cluster, 0, len(ordered))
	shown := make(map[string]bool, len(ordered))
	for _, c := range ordered {
		if len(suppress) > 0 && allSuppressed(c, suppress) {
			out.suppress(recall.SuppressLineageSeen, c)
			continue
		}
		// A near-duplicate here is a cluster that clustering refused to merge —
		// a different record type carrying the same content, say — but which a
		// reader would see as the same text twice.
		prints := fingerprints(c)
		if anyShown(prints, shown) {
			out.suppress(recall.SuppressDuplicate, c)
			continue
		}
		for _, fp := range prints {
			shown[fp] = true
		}
		kept = append(kept, c)
	}

	final, demoted := r.diversify(kept)

	// A per-request limit overrides the configured one. The configured value is
	// a policy default; the request's is what this caller asked for, and a
	// long-lived service serving many callers from one Ranker cannot vary it
	// otherwise.
	limit := r.cfg.Limit
	if req.Limit > 0 {
		limit = req.Limit
	}
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

// allSuppressed reports whether every lineage root in a cluster is one the host
// has already shown. A cluster holding evidence the host has not seen is still
// worth showing, so suppression is all-or-nothing per cluster.
func allSuppressed(c *cluster, suppress map[recall.LineageRoot]bool) bool {
	for _, g := range c.groups {
		if !suppress[g.root] {
			return false
		}
	}
	return true
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

// suppress records a withheld cluster. Count is the number of candidates the
// host is not being shown, so it can say what it is withholding without being
// told what the records were.
func (f *Fusion) suppress(reason string, c *cluster) {
	n := 0
	for _, g := range c.groups {
		n += len(g.candidates)
	}
	f.Suppressed = append(f.Suppressed, recall.Suppression{
		Reason:      reason,
		Count:       n,
		LineageRoot: c.explain.LineageRoot,
	})
}

// result renders a cluster for the response. Members keep one entry per lineage
// group: two members mean two records, and two candidates inside one member
// mean one record seen twice.
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
