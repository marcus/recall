package eval

import (
	"fmt"
	"sort"
	"strings"
)

// PackCoverage describes authored populations without running retrieval. Tags
// reuse the same grouping vocabulary as metric reports and baseline comparison.
type PackCoverage struct {
	Cases     int              `json:"cases"`
	Untagged  int              `json:"untagged"`
	Tags      map[string]int   `json:"tags"`
	Behaviors map[Behavior]int `json:"behaviors"`
}

func CoverageOf(cases []Case) PackCoverage {
	out := PackCoverage{Cases: len(cases), Tags: map[string]int{}, Behaviors: map[Behavior]int{}}
	for _, c := range cases {
		if len(c.Tags) == 0 {
			out.Untagged++
		}
		seen := map[string]bool{}
		for _, tag := range c.Tags {
			if !seen[tag] {
				out.Tags[tag]++
				seen[tag] = true
			}
		}
		out.Behaviors[c.ExpectedBehavior]++
	}
	return out
}

func SummarizeCoverage(c PackCoverage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "coverage: %d cases, %d untagged\n", c.Cases, c.Untagged)
	tags := make([]string, 0, len(c.Tags))
	for tag := range c.Tags {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		fmt.Fprintf(&b, "  tag %s: %d\n", tag, c.Tags[tag])
	}
	for _, behavior := range []Behavior{BehaviorAnswer, BehaviorAbstain, BehaviorFail} {
		fmt.Fprintf(&b, "  behavior %s: %d\n", behavior, c.Behaviors[behavior])
	}
	return b.String()
}
