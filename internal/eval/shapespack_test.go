package eval_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/recall"
)

const shapesPackRel = "../../eval/packs/shapes"

var shapesQueries = map[string]string{
	"mid-chunk-excerpt-001":      "quartz",
	"heading-representative-002": "aurora",
	"link-destination-003":       "lattice",
	"duplicate-views-004":        "orbit",
	"unknown-negative-005":       "xylophonium",
	"question-width-006":         "what is the orbit project for",
}

func shapesLoad(t *testing.T) (*eval.Pack, []eval.Case, []eval.Judgment) {
	t.Helper()
	dir, err := filepath.Abs(shapesPackRel)
	if err != nil {
		t.Fatalf("resolve pack dir: %v", err)
	}
	pack, err := eval.LoadPack(dir)
	if err != nil {
		t.Fatalf("load pack: %v", err)
	}
	cases, err := pack.LoadCases()
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	judgments, err := pack.LoadJudgments()
	if err != nil {
		t.Fatalf("load judgments: %v", err)
	}
	return pack, cases, judgments
}

func shapesCase(t *testing.T, id string) eval.Case {
	t.Helper()
	_, cases, _ := shapesLoad(t)
	for _, c := range cases {
		if c.CaseID == id {
			return c
		}
	}
	t.Fatalf("case %q is missing", id)
	return eval.Case{}
}

func shapesConfigText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(shapesPackRel, "sources", "config.toml"))
	if err != nil {
		t.Fatalf("read pack configuration: %v", err)
	}
	return string(raw)
}

func TestShapesPackLoadsAndValidates(t *testing.T) {
	t.Parallel()
	pack, cases, judgments := shapesLoad(t)
	if err := eval.Validate(pack, cases, judgments); err != nil {
		t.Fatalf("the committed pack does not validate: %v", err)
	}
	if pack.NetworkAccess {
		t.Error("the synthetic pack declares network access")
	}
	if len(cases) != len(shapesQueries) {
		t.Fatalf("cases = %d, want %d regression shapes", len(cases), len(shapesQueries))
	}
	for _, c := range cases {
		if want, ok := shapesQueries[c.CaseID]; !ok || c.Query != want {
			t.Errorf("case %q query = %q, want %q", c.CaseID, c.Query, want)
		}
	}
}

func TestShapesPackClaimsEveryRegressionProperty(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"mid-chunk-excerpt-001":      {"expected_top_lineage", "excerpt_contains"},
		"heading-representative-002": {"expected_top_lineage", "excerpt_contains"},
		"link-destination-003":       {"expected_top_lineage", "max_results"},
		"duplicate-views-004":        {"max_results", "max_results_per_record", "withheld_lineages"},
		"unknown-negative-005":       {"max_results"},
		"question-width-006":         {"max_results", "withheld_lineages"},
	}
	_, cases, _ := shapesLoad(t)
	for _, c := range cases {
		for _, claim := range want[c.CaseID] {
			if !c.Assertions.Declared()[claim] {
				t.Errorf("case %q no longer declares %s", c.CaseID, claim)
			}
		}
		if c.CaseID == "unknown-negative-005" &&
			c.Assertions.ExpectedCoverage != recall.CoverageComplete {
			t.Errorf("honest negative coverage = %q, want complete",
				c.Assertions.ExpectedCoverage)
		}
	}
}

func TestShapesBodylessHeadingKeepsItsRepresentativeShape(t *testing.T) {
	t.Parallel()
	c := shapesCase(t, "heading-representative-002")
	root := recall.LineageRoot("01SHAPESDOCS:guides/aurora-guide.md#L1-L1")
	if !slices.Equal(c.Assertions.ExcerptContains[root], []string{"aurora", "spectrometer"}) {
		t.Errorf("excerpt assertion = %v, want matched body content on the H1 root",
			c.Assertions.ExcerptContains[root])
	}
	raw, err := os.ReadFile(filepath.Join(
		shapesPackRel, "sources", "corpus", "guides", "aurora-guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# Aurora Field Guide\n\n## Observation zone\n\n###") {
		t.Errorf("fixture no longer starts with two body-less headings:\n%s", raw)
	}
}

func TestShapesDuplicateViewsShareOneRecord(t *testing.T) {
	t.Parallel()
	text := shapesConfigText(t)
	if strings.Count(text, `location = "${PACK}/sources/corpus"`) != 2 {
		t.Error("duplicate-view sources no longer read the same synthetic corpus")
	}
	c := shapesCase(t, "duplicate-views-004")
	if c.Assertions.MaxResultsPerRecord == nil || *c.Assertions.MaxResultsPerRecord != 1 {
		t.Errorf("max_results_per_record = %v, want 1", c.Assertions.MaxResultsPerRecord)
	}
	if len(c.Assertions.RequiredSources) != 2 {
		t.Errorf("required_sources = %v, want both configured views", c.Assertions.RequiredSources)
	}
	loser := recall.LineageRoot("02SHAPESVIEW:projects/orbit.md#L1-L5")
	if c.Assertions.WithheldLineages[loser] != string(recall.SuppressDuplicateView) {
		t.Errorf("losing view suppression = %q, want %q",
			c.Assertions.WithheldLineages[loser], recall.SuppressDuplicateView)
	}
}

func TestShapesQuestionDoesNotReachWiderThanItsKeyword(t *testing.T) {
	t.Parallel()
	keyword := shapesCase(t, "duplicate-views-004")
	question := shapesCase(t, "question-width-006")
	if keyword.Assertions.MaxResults == nil || question.Assertions.MaxResults == nil {
		t.Fatal("paired breadth cases must both declare max_results")
	}
	if *question.Assertions.MaxResults > 2**keyword.Assertions.MaxResults {
		t.Errorf("question max_results = %d, more than twice keyword max_results %d",
			*question.Assertions.MaxResults, *keyword.Assertions.MaxResults)
	}
}

func TestShapesFloorCaseTestsMechanicsWithoutCalibratingTheDefault(t *testing.T) {
	t.Parallel()
	text := shapesConfigText(t)
	if strings.Contains(text, "relevance_floor") {
		t.Error("synthetic pack config sets relevance_floor; calibration belongs in a unit test")
	}
	c := shapesCase(t, "question-width-006")
	root := recall.LineageRoot("01SHAPESDOCS:projects/example-queries.md#L1-L5")
	if c.Assertions.WithheldLineages[root] != string(recall.SuppressRelevanceFloor) {
		t.Errorf("quoted example suppression = %q, want %q",
			c.Assertions.WithheldLineages[root], recall.SuppressRelevanceFloor)
	}
}

func TestShapesPackNamesOnlyConfiguredSources(t *testing.T) {
	t.Parallel()
	_, cases, judgments := shapesLoad(t)
	text := shapesConfigText(t)
	named := func(uid recall.SourceUID) bool {
		return strings.Contains(text, `source_uid = "`+string(uid)+`"`)
	}
	uidOf := func(root recall.LineageRoot) recall.SourceUID {
		loc, err := root.Locator()
		if err != nil {
			return ""
		}
		return loc.SourceUID
	}
	for _, j := range judgments {
		if uid := uidOf(j.LineageRoot); uid != "" && !named(uid) {
			t.Errorf("judgment %q names unconfigured source_uid %q", j.CaseID, uid)
		}
	}
	for _, c := range cases {
		if !strings.Contains(text, "[profiles."+c.Profile+"]") {
			t.Errorf("case %q names unconfigured profile %q", c.CaseID, c.Profile)
		}
		for uid := range c.Assertions.ExpectedSourceOutcomes {
			if !named(uid) {
				t.Errorf("case %q names unconfigured source %q", c.CaseID, uid)
			}
		}
	}
}

func TestShapesPackConfigIsPortable(t *testing.T) {
	t.Parallel()
	for _, line := range strings.Split(shapesConfigText(t), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "location = ")
		if ok && !strings.Contains(value, "${PACK}") {
			t.Errorf("location %s is not relative to ${PACK}", value)
		}
	}
}
