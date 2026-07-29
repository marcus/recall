// Package explain renders structured score explanations.
//
// Boundary: rendering only. The explanation is built by internal/ranking as a
// by-product of scoring, so nothing here recomputes a fact or holds one of its
// own. If a value is missing from the output, it is missing from the
// explanation, and the fix belongs upstream.
package explain

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/marcus/recall/pkg/recall"
)

// Render writes an explanation as the compact block the CLI shows under a
// result.
//
// Every populated field appears. That is not a formatting preference: the
// explanation is the product surface for ranking decisions, and a configured
// value that cannot be read off it is indistinguishable from one that never
// applied. A property test in this package asserts the rendering covers the
// struct, so a new field fails the build rather than going quietly missing.
func Render(e recall.Explanation) string {
	var b strings.Builder
	line := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&b, "%-14s %s\n", label+":", value)
	}

	line("source", source(e))
	line("local rank", rank(e))
	line("matches", signals(e.MatchSignals))
	line("prior", prior(e.Prior))
	line("relevance", relevance(e))
	line("lineage", string(e.LineageRoot))
	line("corroboration", corroboration(e.Corroboration))
	line("freshness", freshness(e.Freshness))
	line("reranker", reranker(e.Reranker))
	line("score", score(e))
	return b.String()
}

func source(e recall.Explanation) string {
	switch {
	case e.SourceID != "" && e.SourceUID != "":
		return fmt.Sprintf("%s (%s)", e.SourceID, e.SourceUID)
	case e.SourceID != "":
		return e.SourceID
	default:
		return string(e.SourceUID)
	}
}

func rank(e recall.Explanation) string {
	if e.LocalRank == 0 {
		return ""
	}
	if e.LocalPoolSize > 0 {
		// "rank 3" alone reads as a weak hit when it may have been the whole
		// answer the source had.
		return fmt.Sprintf("%d of %d", e.LocalRank, e.LocalPoolSize)
	}
	return strconv.Itoa(e.LocalRank)
}

func signals(in []recall.MatchSignal) string {
	if len(in) == 0 {
		return ""
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return strings.Join(out, ", ")
}

func prior(p recall.PriorExplanation) string {
	if p.Effective == 0 && p.Base == 0 {
		return ""
	}
	if p.Rule == "" {
		return fmt.Sprintf("%s (base, no class rule)", num(p.Effective))
	}
	// Both the value in force and what the rule changed, so a reader can tell
	// a strong base from a strong adjustment.
	return fmt.Sprintf("%s (base %s, class %q %s)",
		num(p.Effective), num(p.Base), p.Rule, signed(p.Intent))
}

func corroboration(c recall.CorroborationExplanation) string {
	if c.IndependentUnits == 0 {
		return ""
	}
	unit := "units"
	if c.IndependentUnits == 1 {
		unit = "unit"
	}
	out := fmt.Sprintf("%d independent %s", c.IndependentUnits, unit)
	if len(c.Sources) > 0 {
		out += " from " + strings.Join(c.Sources, ", ")
	}
	if c.Cap > 0 {
		out += fmt.Sprintf(" (cap %s", num(c.Cap))
		if c.CapApplied {
			out += ", applied"
		}
		out += ")"
	}
	return out
}

func freshness(f recall.FreshnessExplanation) string {
	var parts []string
	if f.Mode != "" {
		parts = append(parts, string(f.Mode))
	}
	if f.SourceRevision != "" {
		parts = append(parts, "revision "+f.SourceRevision)
	}
	if f.IndexGeneration != "" {
		parts = append(parts, "generation "+f.IndexGeneration)
	}
	if f.IndexModel != "" {
		parts = append(parts, "model "+f.IndexModel)
	}
	if f.IndexConfig != "" {
		parts = append(parts, "index config "+f.IndexConfig)
	}
	if f.ObservedAt != nil {
		parts = append(parts, "observed "+stamp(*f.ObservedAt))
	}
	// Confirmation is the stronger claim: observation alone proves nothing
	// about a later source boundary, so it is never shown as if it did.
	if f.ConfirmedAt != nil {
		parts = append(parts, "confirmed "+stamp(*f.ConfirmedAt))
	}
	if f.AsOfHonored != "" {
		parts = append(parts, "as_of "+string(f.AsOfHonored))
	}
	return strings.Join(parts, ", ")
}

func reranker(r recall.RerankerExplanation) string {
	if !r.Used {
		return "not used"
	}
	out := "used"
	if r.Model != "" {
		out += " (" + r.Model + ")"
	}
	if r.RankBefore > 0 {
		// The pre-rerank position is retained so a routing mistake the reranker
		// papered over stays visible.
		out += fmt.Sprintf(", was rank %d", r.RankBefore)
	}
	if r.Delta != 0 {
		out += ", delta " + signed(r.Delta)
	}
	return out
}

// relevance renders the factor that scaled the prior, and distinguishes a
// source that reported nothing from one that reported a perfect match. Fusion
// reads both as 1.0; they are not the same claim, and only one of them is
// evidence.
func relevance(e recall.Explanation) string {
	if e.Relevance == nil {
		return "not reported by this source, so fused as 1 — an untested advantage over sources that report it"
	}
	return fmt.Sprintf("%s (scales the prior: how much this record is about the query)", num(*e.Relevance))
}

func score(e recall.Explanation) string {
	if e.Score == 0 && e.RankConstant == 0 {
		return ""
	}
	out := num(e.Score)
	if e.RankConstant > 0 {
		out += fmt.Sprintf(" (rank constant %s)", num(e.RankConstant))
	}
	if e.ExactPromoted {
		// A partition, not a bonus, so it is never folded into the number.
		out += ", promoted on exact identifier"
	}
	return out
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

func signed(v float64) string {
	if v >= 0 {
		return "+" + num(v)
	}
	return num(v)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
