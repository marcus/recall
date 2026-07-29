// Package lineage resolves locator identity and computes evidence lineage
// roots.
//
// Boundary: given a candidate pool and a way to map source names to immutable
// identities, it answers one question — which original record does this
// candidate project? Grouping, scoring, and clustering consume the answer and
// live in internal/ranking.
package lineage

import (
	"fmt"
	"sort"

	"github.com/marcus/recall/pkg/recall"
)

// MaxDepth bounds how far declared derivation edges are followed. A chain
// longer than this is a modeling mistake, not a retrieval case, so following
// stops and says so rather than walking indefinitely.
const MaxDepth = 4

// Resolver maps between a source's renameable display name and its immutable
// identity. Configuration implements it; nothing infers a mapping.
type Resolver interface {
	// UID returns the immutable identity of a configured display name.
	UID(sourceID string) (recall.SourceUID, bool)
	// ID returns the display name of an immutable identity.
	ID(uid recall.SourceUID) (string, bool)
}

// MapResolver is a Resolver backed by a display-name-to-identity map.
type MapResolver map[string]recall.SourceUID

func (m MapResolver) UID(sourceID string) (recall.SourceUID, bool) {
	uid, ok := m[sourceID]
	return uid, ok
}

func (m MapResolver) ID(uid recall.SourceUID) (string, bool) {
	// Deterministic under duplicate values, which config validation forbids
	// anyway; sorting keeps a misconfiguration from producing varying output.
	names := make([]string, 0, 2)
	for name, got := range m {
		if got == uid {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false
	}
	sort.Strings(names)
	return names[0], true
}

// ErrNotConfigured reports a locator naming a source this machine does not
// have. It is not a failure of the query: a portable locator may simply refer
// to a source configured elsewhere.
type ErrNotConfigured struct {
	SourceID  string
	SourceUID recall.SourceUID
}

func (e *ErrNotConfigured) Error() string {
	if e.SourceID != "" {
		return fmt.Sprintf("source_not_configured: no source named %q", e.SourceID)
	}
	return fmt.Sprintf("source_not_configured: no source with uid %q", e.SourceUID)
}

// Resolve fills in whichever half of a locator's identity is missing.
func Resolve(r Resolver, loc recall.Locator) (recall.Locator, error) {
	switch {
	case loc.SourceID != "" && loc.SourceUID != "":
		return loc, nil
	case loc.SourceID != "":
		uid, ok := r.UID(loc.SourceID)
		if !ok {
			return loc, &ErrNotConfigured{SourceID: loc.SourceID}
		}
		loc.SourceUID = uid
	case loc.SourceUID != "":
		id, ok := r.ID(loc.SourceUID)
		if !ok {
			return loc, &ErrNotConfigured{SourceUID: loc.SourceUID}
		}
		loc.SourceID = id
	default:
		return loc, fmt.Errorf("%w: locator names no source", recall.ErrMalformedLocator)
	}
	return loc, nil
}

// Lineage is one candidate's place in the derivation graph.
type Lineage struct {
	// Root is the deduplication key: the original record this candidate
	// projects. Candidates sharing a Root are one piece of evidence.
	Root recall.LineageRoot

	// Ancestors are the roots this candidate derives from when it has more than
	// one parent. A composite candidate is its own Root — it projects no single
	// record — but it is not independent of its parents either, so
	// corroboration counting subtracts them.
	Ancestors []recall.LineageRoot

	// Depth is how many edges were followed to reach Root.
	Depth int

	// SourceDerivesFrom is set when this candidate's source declares that it
	// projects another source wholesale and the candidate offered no
	// record-level edge. A source-level edge cannot name which upstream record
	// is projected, so it never changes Root; it only tells corroboration that
	// this evidence is not independent of that source.
	SourceDerivesFrom recall.SourceUID

	// Truncated means following stopped early. Reason says why: "max_depth" or
	// "cycle".
	Truncated bool
	Reason    string

	// Dropped names derived_from edges that could not be resolved, usually
	// because the upstream source is not configured here. The edge is dropped
	// and reported; it never fails the query.
	Dropped []string
}

// Graph computes lineage for a pool of candidates.
//
// Edges are followed within the pool: an edge naming a record that is not in
// the pool terminates there, which is correct — the upstream locator is still
// the original record's identity, whether or not it was also retrieved.
type Graph struct {
	resolver Resolver
	byRoot   map[recall.LineageRoot]recall.Candidate
	// derivesFrom holds source-level fallback edges, keyed by the projecting
	// source. It applies only when a candidate declares no record-level edge.
	derivesFrom map[recall.SourceUID]recall.SourceUID
}

// NewGraph indexes a candidate pool. Candidates must already carry a resolved
// SourceUID; the core attaches it when it receives them from an adapter.
func NewGraph(r Resolver, pool []recall.Candidate) *Graph {
	g := &Graph{
		resolver:    r,
		byRoot:      make(map[recall.LineageRoot]recall.Candidate, len(pool)),
		derivesFrom: make(map[recall.SourceUID]recall.SourceUID),
	}
	for _, c := range pool {
		if self, err := selfRoot(r, c); err == nil {
			g.byRoot[self] = c
		}
	}
	return g
}

// DeclareSourceDerivation records a manifest's source-level derives_from edge.
// It is the fallback used when record-level references are unavailable.
func (g *Graph) DeclareSourceDerivation(from, to recall.SourceUID) {
	g.derivesFrom[from] = to
}

// selfRoot is a candidate's identity before any edge is followed.
func selfRoot(r Resolver, c recall.Candidate) (recall.LineageRoot, error) {
	loc := c.Locator
	if loc.SourceUID == "" {
		loc.SourceUID = c.SourceUID
	}
	if loc.SourceUID == "" && loc.SourceID != "" {
		resolved, err := Resolve(r, loc)
		if err != nil {
			return "", err
		}
		loc = resolved
	}
	return loc.LineageRoot()
}

// Of computes the lineage of one candidate.
func (g *Graph) Of(c recall.Candidate) (Lineage, error) {
	self, err := selfRoot(g.resolver, c)
	if err != nil {
		return Lineage{}, err
	}

	lin := Lineage{Root: self}
	if upstream, ok := g.derivesFrom[c.SourceUID]; ok && len(c.DerivedFrom) == 0 {
		lin.SourceDerivesFrom = upstream
	}
	visited := map[recall.LineageRoot]bool{self: true}
	order := []recall.LineageRoot{self}

	current := c
	for depth := 1; ; depth++ {
		parents, dropped := g.parents(current)
		lin.Dropped = append(lin.Dropped, dropped...)

		if len(parents) == 0 {
			return lin, nil
		}
		if len(parents) > 1 {
			// A composite projects no single record. It keeps its own root and
			// records what it draws on, so corroboration does not count it as
			// independent of its own sources.
			lin.Ancestors = parents
			return lin, nil
		}

		parent := parents[0]
		if visited[parent] {
			// Every member of a cycle must agree on one root, or they would
			// fail to deduplicate. The smallest locator in the cycle is a
			// stable choice available to all of them.
			lin.Root = smallest(append(order, parent))
			lin.Truncated = true
			lin.Reason = "cycle"
			return lin, nil
		}
		if depth > MaxDepth {
			lin.Truncated = true
			lin.Reason = "max_depth"
			return lin, nil
		}

		visited[parent] = true
		order = append(order, parent)
		lin.Root = parent
		lin.Depth = depth

		next, ok := g.byRoot[parent]
		if !ok {
			// The upstream record was not retrieved. Its locator is still the
			// original record's identity, so the chain ends here correctly.
			return lin, nil
		}
		current = next
	}
}

// parents returns the resolved roots a candidate derives from, plus any edges
// that could not be resolved.
func (g *Graph) parents(c recall.Candidate) (roots []recall.LineageRoot, dropped []string) {
	for _, edge := range c.DerivedFrom {
		resolved, err := Resolve(g.resolver, edge)
		if err != nil {
			dropped = append(dropped, edge.String())
			continue
		}
		root, err := resolved.LineageRoot()
		if err != nil {
			dropped = append(dropped, edge.String())
			continue
		}
		roots = append(roots, root)
	}
	return roots, dropped
}

// sourceOf recovers the immutable source identity encoded in a lineage root.
func sourceOf(root recall.LineageRoot) recall.SourceUID {
	loc, err := root.Locator()
	if err != nil {
		return ""
	}
	return loc.SourceUID
}

func smallest(roots []recall.LineageRoot) recall.LineageRoot {
	out := roots[0]
	for _, r := range roots[1:] {
		if r < out {
			out = r
		}
	}
	return out
}

// Independent counts how much independent evidence a set of lineages
// represents. It is the input to corroboration, so it is deliberately
// conservative: everything that might be a projection of something else
// already counted is excluded.
//
// Three things reduce the count. Lineages sharing a root are one record.
// A composite drawing on an already-counted root is not independent of it.
// A candidate from a source that declares it projects another already-counted
// source is not independent of that source.
func Independent(lineages []Lineage) int {
	roots := make(map[recall.LineageRoot]Lineage, len(lineages))
	sources := make(map[recall.SourceUID]bool, len(lineages))
	for _, l := range lineages {
		// Keep the shallowest lineage for a root: it carries the most direct
		// account of where the record came from.
		if prev, seen := roots[l.Root]; !seen || l.Depth < prev.Depth {
			roots[l.Root] = l
		}
		sources[sourceOf(l.Root)] = true
	}

	n := 0
	for _, l := range roots {
		if derivesFromCounted(l, roots) {
			continue
		}
		if l.SourceDerivesFrom != "" && sources[l.SourceDerivesFrom] {
			continue
		}
		n++
	}
	return n
}

func derivesFromCounted(l Lineage, counted map[recall.LineageRoot]Lineage) bool {
	for _, a := range l.Ancestors {
		if _, ok := counted[a]; ok {
			return true
		}
	}
	return false
}
