package eval

import (
	"strings"
	"testing"
)

func TestCoverageCountsCasesOncePerTag(t *testing.T) {
	c := CoverageOf([]Case{
		{Tags: []string{"work", "family:lookup", "work"}, ExpectedBehavior: BehaviorAnswer},
		{Tags: []string{"home", "family:lookup"}, ExpectedBehavior: BehaviorAbstain},
		{ExpectedBehavior: BehaviorFail},
	})
	if c.Cases != 3 || c.Untagged != 1 || c.Tags["work"] != 1 || c.Tags["family:lookup"] != 2 || c.Behaviors[BehaviorAbstain] != 1 {
		t.Fatalf("coverage: %+v", c)
	}
	text := SummarizeCoverage(c)
	if !strings.Contains(text, "tag family:lookup: 2") || strings.Index(text, "tag home") > strings.Index(text, "tag work") {
		t.Fatalf("summary: %s", text)
	}
	empty := CoverageOf(nil)
	if empty.Cases != 0 || len(empty.Tags) != 0 {
		t.Fatalf("empty: %+v", empty)
	}
}
