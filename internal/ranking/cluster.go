package ranking

import (
	"cmp"
	"slices"
	"strings"
	"unicode"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/recall"
)

// Metadata keys adapters use to declare identity for clustering. They are the
// whole vocabulary: fusion never infers identity from prose, so an adapter that
// knows two records are the same thing has to say so here.
const (
	// MetaEntityID is a stable identifier for the entity a candidate is about,
	// meaningful across sources. A contact record and a calendar attendee
	// carrying the same MetaEntityID are the same person.
	MetaEntityID = "entity_id"

	// MetaEntityType scopes MetaEntityID. It defaults to the record type, so
	// person "42" and task "42" never collide.
	MetaEntityType = "entity_type"

	// MetaAliases lists declared alternate names for the entity, as []string.
	// An alias matches another candidate's name only in full, at token
	// boundaries, never as a substring.
	MetaAliases = "aliases"
)

// minNameTokens is the floor for matching two candidates by name. A single word
// is not identity: "Recall", "Meeting", and "notes" name many different things,
// and an adapter that really knows the identity of a one-word entity declares
// MetaEntityID instead.
const minNameTokens = 2

// cluster is a set of lineage groups about one entity, event, or fact. It is
// the display and corroboration unit.
type cluster struct {
	groups []*group

	// units are the groups collapsed by duplicate identity: two chunks of one
	// document, or two records with the same content fingerprint, are one unit.
	// Corroboration counts units, not groups, or a source that chunks finely
	// would corroborate itself.
	units []unit

	primary  recall.Candidate
	explain  recall.Explanation
	score    float64
	exact    bool
	maxGroup float64
}

// unit is one piece of evidence: the lineage groups that are the same record
// seen more than once.
type unit struct {
	groups   []*group
	lineages []lineage.Lineage
	score    float64
	root     recall.LineageRoot

	// derivative marks a unit every view of which restates something else — a
	// composite of records it names, or a source declaring it mirrors another.
	// Corroboration considers such units last, so a strong restatement cannot
	// take the place of the records it restates.
	derivative bool
}

// clusterGroups performs step 4: merge lineage groups referring to the same
// entity, event, or fact, then score the merged cluster.
//
// Merging is by declared identity in strength order — a stable entity
// identifier, then the same record seen again, then a conservative full-name
// match, then an advisory content fingerprint. Every rule compares whole
// values; none compares a substring of one against another.
func (r *Ranker) clusterGroups(groups []*group) []*cluster {
	dup := newDisjoint(len(groups))  // same record, seen again
	same := newDisjoint(len(groups)) // same subject

	byKey := make(map[string]int, len(groups)*2)
	link := func(i int, key string, sets ...*disjoint) {
		first, seen := byKey[key]
		if !seen {
			byKey[key] = i
			return
		}
		for _, s := range sets {
			s.union(first, i)
		}
	}
	for i, g := range groups {
		for _, key := range g.duplicateKeys() {
			// A duplicate is also the same subject, so it links both relations.
			link(i, key, dup, same)
		}
		for _, key := range g.entityKeys() {
			link(i, key, same)
		}
	}

	clusters := make([]*cluster, 0, len(groups))
	byRep := make(map[int]*cluster, len(groups))
	for i, g := range groups {
		rep := same.find(i)
		c := byRep[rep]
		if c == nil {
			c = &cluster{}
			byRep[rep] = c
			clusters = append(clusters, c)
		}
		c.groups = append(c.groups, g)
		c.exact = c.exact || g.exact
	}

	for _, c := range clusters {
		c.units = collapse(c.groups, dup, groups)
		r.scoreCluster(c)
	}
	return clusters
}

// collapse folds a cluster's groups into corroboration units using the
// duplicate relation.
func collapse(members []*group, dup *disjoint, all []*group) []unit {
	index := make(map[*group]int, len(all))
	for i, g := range all {
		index[g] = i
	}

	byRep := make(map[int]*unit, len(members))
	built := make([]*unit, 0, len(members))
	for _, g := range members {
		rep := dup.find(index[g])
		u := byRep[rep]
		if u == nil {
			u = &unit{root: g.root}
			byRep[rep] = u
			built = append(built, u)
		}
		u.groups = append(u.groups, g)
		u.lineages = append(u.lineages, g.lineages...)
		u.derivative = derivative(u.lineages)
		// A unit is one record, so its weight is its best view of that record,
		// exactly as a lineage group takes the max over its sources.
		if g.score > u.score {
			u.score = g.score
		}
		if g.root < u.root {
			u.root = g.root
		}
	}

	units := make([]unit, 0, len(built))
	for _, u := range built {
		units = append(units, *u)
	}

	// Strongest first, then root, so corroboration selection and the choice of
	// primary do not depend on adapter return order.
	slices.SortFunc(units, func(a, b unit) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.root, b.root))
	})
	return units
}

// scoreCluster computes
//
//	cluster_score = min(sum over independent units, corroboration_cap * max unit)
//
// and finishes the explanation the group stage started. The sum runs over units
// that [lineage.Independent] agrees are new evidence: a composite restating two
// records it derives from, or a source that declares it mirrors another, adds
// display value but not corroboration.
func (r *Ranker) scoreCluster(c *cluster) {
	var counted []lineage.Lineage
	var sum float64
	independent, corroborating := 0, 0
	sources := make(map[string]bool, len(c.units))

	for _, u := range corroborationOrder(c.units) {
		trial := append(slices.Clone(counted), u.lineages...)
		got := lineage.Independent(trial)
		if got <= independent {
			// This unit is a projection of evidence already counted. It stays in
			// the cluster for display; it does not add to the score.
			continue
		}
		counted, independent = trial, got
		// Units are counted, not lineages: a record that arrived as three chunks
		// raises the lineage count by three and the evidence count by one.
		corroborating++
		sum += u.score
		for _, g := range u.groups {
			for _, cand := range g.candidates {
				sources[cand.SourceID] = true
			}
		}
	}

	// A cluster exists only because a group joined it, so there is always at
	// least one unit, and the units are sorted strongest first.
	c.maxGroup = c.units[0].score
	capped := r.cfg.CorroborationCap * c.maxGroup
	c.score = sum
	capApplied := sum > capped
	if capApplied {
		c.score = capped
	}

	// The primary group of a cluster is its strongest unit's strongest group.
	primary := c.units[0].groups[0]
	for _, g := range c.units[0].groups {
		if g.score > primary.score || (g.score == primary.score && g.root < primary.root) {
			primary = g
		}
	}
	c.primary = primary.primary
	c.explain = primary.explain
	c.explain.Corroboration = recall.CorroborationExplanation{
		DistinctLineages: corroborating,
		Sources:          sortedNames(sources),
		Cap:              r.cfg.CorroborationCap,
		CapApplied:       capApplied,
	}
	c.explain.Score = c.score
	c.explain.ExactPromoted = c.exact
}

// corroborationOrder is the order units are offered to the corroboration
// count. Records that restate other records come last, whatever they scored:
// otherwise a weekly summary that ranked first would be counted as the
// evidence, and the two tasks it summarizes would be rejected as restatements
// of it. The order affects which units are counted, never the arithmetic.
func corroborationOrder(units []unit) []unit {
	out := slices.Clone(units)
	slices.SortFunc(out, func(a, b unit) int {
		return cmp.Or(
			compareBool(a.derivative, b.derivative),
			cmp.Compare(b.score, a.score),
			cmp.Compare(a.root, b.root),
		)
	})
	return out
}

// derivative reports whether every view of a record restates something else.
// One direct view is enough to make the record evidence in its own right.
func derivative(lineages []lineage.Lineage) bool {
	for _, l := range lineages {
		if len(l.Ancestors) == 0 && l.SourceDerivesFrom == "" {
			return false
		}
	}
	return len(lineages) > 0
}

// compareBool sorts false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
}

// duplicateKeys identify a record seen again. Groups sharing one are one piece
// of evidence and must never corroborate each other.
func (g *group) duplicateKeys() []string {
	var keys []string
	for _, c := range g.candidates {
		// A record identifier is only unique inside its own source, so it is
		// always scoped by source. Two sources numbering their records from 1
		// must not collide.
		if c.SourceRecordID != "" && c.SourceUID != "" {
			keys = append(keys, key("record", string(c.SourceUID), c.SourceRecordID))
		}
		// The fingerprint is advisory: it collapses duplicates for scoring, and
		// the candidates still expand separately. It is scoped by record type so
		// a task and a document with identical text stay distinct records.
		if c.ContentFingerprint != "" {
			keys = append(keys, key("fingerprint", string(c.RecordType), c.ContentFingerprint))
		}
	}
	return dedupe(keys)
}

// entityKeys identify the same subject reached independently. Groups sharing
// one cluster together and do corroborate each other.
func (g *group) entityKeys() []string {
	var keys []string
	for _, c := range g.candidates {
		if id := metaString(c.Metadata, MetaEntityID); id != "" {
			typ := metaString(c.Metadata, MetaEntityType)
			if typ == "" {
				typ = string(c.RecordType)
			}
			keys = append(keys, key("entity", typ, id))
		}
		for _, name := range matchableNames(c) {
			keys = append(keys, key("name", string(c.RecordType), name))
		}
	}
	return dedupe(keys)
}

// matchableNames is the conservative fallback when no identifier was declared:
// a candidate's title and its declared aliases, normalized. A name matches only
// another whole name, and only when it carries at least [minNameTokens] tokens.
// There is no substring test anywhere in this path.
func matchableNames(c recall.Candidate) []string {
	raw := make([]string, 0, 4)
	raw = append(raw, c.Title)
	raw = append(raw, metaStrings(c.Metadata, MetaAliases)...)

	out := make([]string, 0, len(raw))
	for _, s := range raw {
		name, tokens := normalizeName(s)
		if tokens >= minNameTokens {
			out = append(out, name)
		}
	}
	return out
}

// normalizeName folds case and reduces a name to space-separated alphanumeric
// tokens, so "Marcus  Vorwaller" and "marcus vorwaller" are one name while
// "marcus" and "marcus vorwaller" remain two.
func normalizeName(s string) (name string, tokens int) {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(fields, " "), len(fields)
}

// key joins parts with a separator that cannot occur in any of them, so no
// combination of values can forge another key.
func key(parts ...string) string { return strings.Join(parts, "\x00") }

func metaString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func metaStrings(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	slices.Sort(in)
	return slices.Compact(in)
}

func sortedNames(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// disjoint is a union-find over group indexes. Merge rules are commutative, so
// the resulting partition does not depend on the order candidates arrived in.
type disjoint struct{ parent []int }

func newDisjoint(n int) *disjoint {
	d := &disjoint{parent: make([]int, n)}
	for i := range d.parent {
		d.parent[i] = i
	}
	return d
}

func (d *disjoint) find(i int) int {
	for d.parent[i] != i {
		d.parent[i] = d.parent[d.parent[i]]
		i = d.parent[i]
	}
	return i
}

func (d *disjoint) union(a, b int) {
	ra, rb := d.find(a), d.find(b)
	if ra == rb {
		return
	}
	// Lowest index wins so the representative is a function of the partition,
	// not of merge order.
	if rb < ra {
		ra, rb = rb, ra
	}
	d.parent[rb] = ra
}
