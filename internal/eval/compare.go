package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Delta is one metric's movement between two runs.
type Delta struct {
	Metric string `json:"metric"`

	// Key is a stable, dimension-qualified population identity. Dimension,
	// Population, and Group retain its structured form for machines that
	// should not have to parse it.
	Key        string `json:"key"`
	Dimension  string `json:"dimension"`
	Population string `json:"population"`
	Group      string `json:"group,omitempty"`

	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Change    float64 `json:"change"`

	// Defined is false when either side had nothing to average. A metric that
	// went from undefined to defined has not improved; it has started being
	// measured, and reporting a change would be inventing one.
	Defined bool `json:"defined"`
}

// CaseChange is one case that started or stopped passing.
type CaseChange struct {
	CaseID string `json:"case_id"`
	Metric string `json:"metric"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// Comparison is what changed between a baseline and a candidate.
type Comparison struct {
	BaselineRun  string `json:"baseline_run"`
	CandidateRun string `json:"candidate_run"`

	// Comparable is false when the two runs did not measure the same thing.
	// Everything below is then unreliable and must not be read as a result.
	Comparable bool     `json:"comparable"`
	Blockers   []string `json:"blockers,omitempty"`

	Overall        []Delta      `json:"overall"`
	ByTag          []Delta      `json:"by_tag,omitempty"`
	BySourceFamily []Delta      `json:"by_source_family,omitempty"`
	Cases          []CaseChange `json:"cases,omitempty"`

	// Regressions names every overall, group, or macro population that got
	// worse. A gain in the aggregate that hides a loss in a group is the
	// failure mode grouped reporting exists to expose, so it is surfaced
	// rather than left to be noticed.
	Regressions []Delta `json:"regressions,omitempty"`
}

// Acceptable reports whether the candidate may be promoted over the baseline.
//
// It is a method rather than something each caller re-derives, because the CLI
// and CI both have to answer it the same way: a comparison that is not evidence
// and a comparison that is evidence of a loss are the same verdict.
func (c Comparison) Acceptable() bool {
	return c.Comparable && len(c.Regressions) == 0
}

// floatNoise is how far a rate may move down before it counts as a regression.
//
// It is not slack for real movement. The smoke pack is deterministic, and the
// smallest change one case can make to a forty-case average is around 1e-3 —
// six orders of magnitude above this. It exists so that a difference in the
// last bit of a float, from a compiler free to fuse a multiply and an add or a
// baseline frozen on one architecture and checked on another, is not reported
// as a quality regression. A report that cries wolf over 1e-17 is a report
// people learn to skip.
const floatNoise = 1e-9

// Compare diffs two runs.
//
// It refuses to compare runs that did not measure the same thing. Two runs over
// different pack content, or with a gate failure in either, are not a before
// and after — reporting a delta between them would be a number with no meaning
// that somebody would nonetheless act on.
func Compare(baseline, candidate Run, baseScores, candScores []CaseScore) Comparison {
	c := Comparison{
		BaselineRun:  baseline.RunID,
		CandidateRun: candidate.RunID,
		Comparable:   true,
	}

	if baseline.Pack.ContentHash != candidate.Pack.ContentHash {
		c.Comparable = false
		c.Blockers = append(c.Blockers, fmt.Sprintf(
			"pack content differs (%s vs %s): the runs measured different questions",
			short(baseline.Pack.ContentHash), short(candidate.Pack.ContentHash)))
	}
	if baseline.Status == StatusInvalid {
		c.Comparable = false
		c.Blockers = append(c.Blockers, "baseline run failed a gate, so it is not evidence")
	}
	if candidate.Status == StatusInvalid {
		c.Comparable = false
		c.Blockers = append(c.Blockers, "candidate run failed a gate, so it is not evidence")
	}
	if baseline.Environment.Arch != candidate.Environment.Arch ||
		baseline.Environment.OS != candidate.Environment.OS {
		c.Blockers = append(c.Blockers, fmt.Sprintf(
			"different hosts (%s/%s vs %s/%s): latency is not comparable",
			baseline.Environment.OS, baseline.Environment.Arch,
			candidate.Environment.OS, candidate.Environment.Arch))
	}

	c.Overall = ratesDelta(
		"overall", "overall", "overall", "",
		baseline.Metrics.Overall.Rates, candidate.Metrics.Overall.Rates)
	c.ByTag = groupDeltas("tag", baseline.Metrics.ByTag, candidate.Metrics.ByTag)
	c.BySourceFamily = groupDeltas(
		"source_family",
		baseline.Metrics.BySourceFamily,
		candidate.Metrics.BySourceFamily,
	)

	// A baseline frozen as a run record on its own carries no per-case detail:
	// cases.jsonl holds excerpts and never enters the repository. Diffing
	// against an absent baseline would report every case in the candidate as
	// newly appeared, burying the metric deltas that are the whole point under
	// a page of noise about nothing having changed.
	if len(baseScores) > 0 {
		c.Cases = caseChanges(baseScores, candScores)
	}

	for _, deltas := range [][]Delta{c.Overall, c.ByTag, c.BySourceFamily} {
		for _, d := range deltas {
			if d.Defined && d.Change < -floatNoise {
				c.Regressions = append(c.Regressions, d)
			}
		}
	}
	return c
}

func ratesDelta(key, dimension, population, group string, base, cand Rates) []Delta {
	var out []Delta
	for _, f := range rateFields() {
		b, cv := *f.mean(&base), *f.mean(&cand)
		d := Delta{
			Metric:     f.name,
			Key:        key,
			Dimension:  dimension,
			Population: population,
			Group:      group,
			Baseline:   b.Value,
			Candidate:  cv.Value,
			Defined:    b.Defined() && cv.Defined(),
		}
		if d.Defined {
			d.Change = cv.Value - b.Value
			// Forbidden evidence is the one rate where less is better, so its
			// sign is flipped to keep "negative change means worse" true
			// everywhere a reader looks. Zero is left alone because negating it
			// yields negative zero, which renders as "-0.0000" and reads as a
			// loss on the one line most runs will show here.
			if f.name == "forbidden_at_5" && d.Change != 0 {
				d.Change = -d.Change
			}
		}
		out = append(out, d)
	}
	return out
}

func groupDeltas(dimension string, base, cand GroupReport) []Delta {
	var out []Delta
	names := map[string]bool{}
	for k := range base.Groups {
		names[k] = true
	}
	for k := range cand.Groups {
		names[k] = true
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		out = append(out, ratesDelta(
			dimension+":group:"+name,
			dimension,
			"group",
			name,
			base.Groups[name].Rates,
			cand.Groups[name].Rates,
		)...)
	}
	out = append(out, ratesDelta(
		dimension+":macro",
		dimension,
		"macro",
		"",
		base.Macro.Rates,
		cand.Macro.Rates,
	)...)
	return out
}

// caseChanges names the cases that started or stopped succeeding.
//
// An aggregate says how much moved; this says what moved, which is the only
// form a reviewer can check against the change under test.
func caseChanges(base, cand []CaseScore) []CaseChange {
	byID := make(map[string]CaseScore, len(base))
	for _, s := range base {
		byID[s.CaseID] = s
	}

	var out []CaseChange
	for _, c := range cand {
		b, ok := byID[c.CaseID]
		if !ok {
			out = append(out, CaseChange{CaseID: c.CaseID, Metric: "case", From: "absent", To: "present"})
			continue
		}
		for _, m := range []struct {
			name     string
			from, to Value
		}{
			{"success_at_5", b.Success5, c.Success5},
			{"abstention_accuracy", b.AbstentionAccuracy, c.AbstentionAccuracy},
			{"locator_success", b.LocatorSuccess, c.LocatorSuccess},
			{"forbidden_at_5", b.Forbidden5, c.Forbidden5},
		} {
			if m.from.OK && m.to.OK && m.from.V != m.to.V {
				out = append(out, CaseChange{
					CaseID: c.CaseID,
					Metric: m.name,
					From:   fmt.Sprintf("%.4g", m.from.V),
					To:     fmt.Sprintf("%.4g", m.to.V),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CaseID != out[j].CaseID {
			return out[i].CaseID < out[j].CaseID
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

// SummarizeComparison renders a comparison for a person.
func SummarizeComparison(c Comparison) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s vs %s\n\n", short(c.BaselineRun), short(c.CandidateRun))

	// The verdict leads. A reader who stops after one line must not stop on a
	// number that turned out not to be evidence, or on a table whose one
	// downward row was three screens further down.
	switch {
	case !c.Comparable:
		b.WriteString("**Not comparable.** These runs did not measure the same thing, ")
		b.WriteString("so the numbers below are not a before and after.\n\n")
	case len(c.Regressions) == 1:
		b.WriteString("**Regressed.** One measurement moved down against the baseline. ")
		b.WriteString("It is named under Regressions below, and the exit status says the same thing.\n\n")
	case len(c.Regressions) > 1:
		fmt.Fprintf(&b, "**Regressed.** %d measurements moved down against the baseline. "+
			"They are named under Regressions below, and the exit status says the same thing.\n\n",
			len(c.Regressions))
	default:
		b.WriteString("**No regression.** Nothing measured moved down.\n\n")
	}

	for _, blocker := range c.Blockers {
		fmt.Fprintf(&b, "- %s\n", blocker)
	}
	if len(c.Blockers) > 0 {
		b.WriteString("\n")
	}

	b.WriteString("## Overall\n\n")
	for _, d := range c.Overall {
		if !d.Defined {
			fmt.Fprintf(&b, "- %s: n/a\n", d.Metric)
			continue
		}
		fmt.Fprintf(&b, "- %s: %.4f → %.4f (%+.4f)\n", d.Metric, d.Baseline, d.Candidate, d.Change)
	}

	if len(c.Regressions) > 0 {
		b.WriteString("\n## Regressions\n\n")
		b.WriteString("A gain in the aggregate does not excuse any of these.\n\n")
		for _, d := range c.Regressions {
			fmt.Fprintf(&b, "- %s: %+.4f\n", deltaLabel(d), d.Change)
		}
	}

	if len(c.Cases) > 0 {
		b.WriteString("\n## Cases that changed\n\n")
		for _, cc := range c.Cases {
			fmt.Fprintf(&b, "- `%s` %s: %s → %s\n", cc.CaseID, cc.Metric, cc.From, cc.To)
		}
	}
	return b.String()
}

func deltaLabel(d Delta) string {
	switch d.Population {
	case "overall":
		return d.Metric
	case "macro":
		return strings.ReplaceAll(d.Dimension, "_", " ") + " macro/" + d.Metric
	case "group":
		return fmt.Sprintf("%s group %q/%s",
			strings.ReplaceAll(d.Dimension, "_", " "), d.Group, d.Metric)
	default:
		return d.Key + "/" + d.Metric
	}
}
