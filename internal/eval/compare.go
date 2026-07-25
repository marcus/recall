package eval

import (
	"fmt"
	"sort"
	"strings"
)

// Delta is one metric's movement between two runs.
type Delta struct {
	Metric    string  `json:"metric"`
	Group     string  `json:"group,omitempty"`
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

	Overall []Delta      `json:"overall"`
	ByTag   []Delta      `json:"by_tag,omitempty"`
	Cases   []CaseChange `json:"cases,omitempty"`

	// Regressions names the groups that got worse. A gain in the aggregate that
	// hides a loss in a group is the failure mode macro averaging exists to
	// expose, so it is surfaced rather than left to be noticed.
	Regressions []Delta `json:"regressions,omitempty"`
}

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

	c.Overall = ratesDelta("", baseline.Metrics.Overall.Rates, candidate.Metrics.Overall.Rates)
	c.ByTag = groupDeltas(baseline.Metrics.ByTag, candidate.Metrics.ByTag)
	c.Cases = caseChanges(baseScores, candScores)

	for _, d := range append(append([]Delta{}, c.Overall...), c.ByTag...) {
		if d.Defined && d.Change < 0 {
			c.Regressions = append(c.Regressions, d)
		}
	}
	return c
}

func ratesDelta(group string, base, cand Rates) []Delta {
	var out []Delta
	for _, f := range rateFields() {
		b, cv := *f.mean(&base), *f.mean(&cand)
		d := Delta{
			Metric:    f.name,
			Group:     group,
			Baseline:  b.Value,
			Candidate: cv.Value,
			Defined:   b.Defined() && cv.Defined(),
		}
		if d.Defined {
			d.Change = cv.Value - b.Value
			// Forbidden evidence is the one rate where less is better, so its
			// sign is flipped to keep "negative change means worse" true
			// everywhere a reader looks.
			if f.name == "forbidden_at_5" {
				d.Change = -d.Change
			}
		}
		out = append(out, d)
	}
	return out
}

func groupDeltas(base, cand GroupReport) []Delta {
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
		out = append(out, ratesDelta(name, base.Groups[name].Rates, cand.Groups[name].Rates)...)
	}
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

	if !c.Comparable {
		b.WriteString("**Not comparable.** These runs did not measure the same thing, ")
		b.WriteString("so the numbers below are not a before and after.\n\n")
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
			label := d.Metric
			if d.Group != "" {
				label = d.Group + "/" + d.Metric
			}
			fmt.Fprintf(&b, "- %s: %+.4f\n", label, d.Change)
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
