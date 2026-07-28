package eval_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/internal/recall"
)

// These tests are about the committed first-use pack rather than about this
// package's types. It is the one committed pack whose corpus is real, and it
// gates every change to ranking and admission, so the ways it can quietly stop
// meaning anything are worth naming: a query edited until it passes, an
// expected-failure marking with nothing behind it, a judgment naming a source
// the pack does not configure, or a machine path in the configuration.
//
// The pack cannot be executed here — `recall eval run` is built separately —
// so what these cover is structure and provenance. Its behaviour is measured
// by `make eval` against eval/baselines/firstuse.json.

const firstUsePackRel = "../../eval/packs/firstuse"

// firstUseQueries is the session of 2026-07-27, verbatim.
//
// It is written out here so that editing a query is a test change and not a
// quiet one. A real question reformulated until it passes deletes the finding
// the pack exists to preserve, and these six are the whole reason the pack is
// not synthetic.
var firstUseQueries = map[string]string{
	"dentist-001":         "dentist",
	"bonnie-002":          "bonnie",
	"fujifilm-003":        "fujifilm",
	"sidecar-004":         "sidecar",
	"blog-005":            "blog",
	"sidecar-natural-006": "what is the sidecar project for",
}

func firstUseLoad(t *testing.T) (*eval.Pack, []eval.Case, []eval.Judgment) {
	t.Helper()
	dir, err := filepath.Abs(firstUsePackRel)
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

func firstUseConfigText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(firstUsePackRel, "sources", "config.toml"))
	if err != nil {
		t.Fatalf("read pack configuration: %v", err)
	}
	return string(raw)
}

func TestFirstUsePackLoadsAndValidates(t *testing.T) {
	t.Parallel()
	pack, cases, judgments := firstUseLoad(t)

	if err := eval.Validate(pack, cases, judgments); err != nil {
		t.Fatalf("the committed pack does not validate: %v", err)
	}
	if pack.NetworkAccess {
		t.Error("the pack declares network access; every source it configures answers from a recording")
	}
}

// The six queries are the pack's subject. Quietly editing one until it passes
// would leave the pack green and the finding gone.
func TestFirstUsePackAsksTheSessionsQuestions(t *testing.T) {
	t.Parallel()
	_, cases, _ := firstUseLoad(t)

	if len(cases) != len(firstUseQueries) {
		t.Fatalf("cases = %d, want the session's %d", len(cases), len(firstUseQueries))
	}
	for _, c := range cases {
		want, known := firstUseQueries[c.CaseID]
		if !known {
			t.Errorf("case %q is not one of the session's queries", c.CaseID)
			continue
		}
		if c.Query != want {
			t.Errorf("case %q asks %q, the session asked %q", c.CaseID, c.Query, want)
		}
	}
}

// An expected failure is a claim about a defect, so it has to say which one
// and name what it excuses. It must also leave something enforced: a marking
// that excuses every assertion the case declares is a case-wide exemption
// wearing a list, and the next regression on that case rides in behind it.
func TestFirstUseExpectedFailuresNameWhatTheyExcuseAndLeaveTheRestEnforced(t *testing.T) {
	t.Parallel()
	_, cases, _ := firstUseLoad(t)

	marked := 0
	for _, c := range cases {
		if c.ExpectedFail == nil {
			continue
		}
		marked++
		if len(c.ExpectedFail.Reason) < 40 {
			t.Errorf("case %q: expected_fail reason %q does not say what it is waiting on",
				c.CaseID, c.ExpectedFail.Reason)
		}
		if len(c.ExpectedFail.Assertions) == 0 {
			t.Errorf("case %q excuses nothing by name, so it excuses nothing", c.CaseID)
			continue
		}
		declared := c.Assertions.Declared()
		if len(declared) <= len(c.ExpectedFail.Assertions) {
			t.Errorf("case %q excuses every assertion it declares (%v of %v); a marking has to "+
				"leave something enforced or an unrelated regression passes behind it",
				c.CaseID, c.ExpectedFail.Assertions, declared)
		}
	}
	if marked == 0 {
		t.Fatal("no case is marked expected_fail; the pack was written because four of them fail")
	}
}

// The two sidecar cases are the session's duplication finding, in its keyword
// and its sentence form. td-87eecf fused two views of one catalog into one
// result and both now hold, so the claims that were excused while it was open
// are the ones that must stay declared: a count bound and a per-record bound
// the fix is measured by, and which nothing else in the pack states.
func TestFirstUseSidecarCasesStillClaimWhatTheDuplicateFixDelivered(t *testing.T) {
	t.Parallel()
	_, cases, _ := firstUseLoad(t)

	want := map[string][]string{
		"sidecar-004":         {"max_results_per_record"},
		"sidecar-natural-006": {"max_results", "max_results_per_record"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		claims, watched := want[c.CaseID]
		if !watched {
			continue
		}
		seen[c.CaseID] = true
		declared := c.Assertions.Declared()
		for _, name := range claims {
			if !declared[name] {
				t.Errorf("case %q no longer declares %s, which is what the duplicate fix is measured by",
					c.CaseID, name)
			}
			if c.ExpectedFail != nil && slices.Contains(c.ExpectedFail.Assertions, name) {
				t.Errorf("case %q excuses %s again; it has held since td-87eecf", c.CaseID, name)
			}
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("case %q is missing", id)
		}
	}
}

// Four cases carry no expected-failure marking, and they are the reason the
// pack is a regression guard rather than a bug list: two are what the session
// found working, and two are what it found broken and a ticket has since
// fixed. The ranking changes this pack gates are what could break either.
func TestFirstUsePackGuardsWhatAlreadyWorks(t *testing.T) {
	t.Parallel()
	_, cases, _ := firstUseLoad(t)

	guards := map[string]bool{}
	for _, c := range cases {
		if c.ExpectedFail == nil {
			guards[c.CaseID] = true
		}
	}
	for _, id := range []string{"bonnie-002", "fujifilm-003", "sidecar-004", "sidecar-natural-006"} {
		if !guards[id] {
			t.Errorf("case %q is marked expected_fail; it passes today and must keep passing", id)
		}
	}
}

func TestFirstUsePackNamesOnlyConfiguredSources(t *testing.T) {
	t.Parallel()
	_, cases, judgments := firstUseLoad(t)
	text := firstUseConfigText(t)

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
			t.Errorf("judgment %q names source_uid %q, which the pack does not configure",
				j.CaseID, uid)
		}
	}
	for _, c := range cases {
		if !strings.Contains(text, "[profiles."+c.Profile+"]") {
			t.Errorf("case %q names profile %q, which the pack does not configure",
				c.CaseID, c.Profile)
		}
		if c.Assertions == nil {
			continue
		}
		for uid := range c.Assertions.ExpectedSourceOutcomes {
			if !named(uid) {
				t.Errorf("case %q asserts an outcome for unconfigured source %q", c.CaseID, uid)
			}
		}
		for _, uid := range append(append([]recall.SourceUID(nil),
			c.Assertions.RequiredSources...), c.Assertions.ForbiddenSources...) {
			if !named(uid) {
				t.Errorf("case %q names unconfigured source %q", c.CaseID, uid)
			}
		}
		for _, root := range []recall.LineageRoot{c.Assertions.ExpectedTopLineage} {
			if root != "" && !named(uidOf(root)) {
				t.Errorf("case %q expects an unconfigured source at the top: %q", c.CaseID, root)
			}
		}
		for root := range c.Assertions.ExcerptContains {
			if !named(uidOf(root)) {
				t.Errorf("case %q reads an excerpt from unconfigured source %q", c.CaseID, root)
			}
		}
	}
}

// The rule that lets a pack of fixture sources be committed at all: no machine
// paths, in the configuration or in the fixtures it points at. The two ongoing
// sources are the exception the runner's network policy exists for — they name
// an endpoint that cannot resolve and answer from a recording — so they are
// checked for that shape rather than for ${PACK}.
func TestFirstUsePackConfigIsPortable(t *testing.T) {
	t.Parallel()
	text := firstUseConfigText(t)

	for _, line := range strings.Split(text, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "location = ")
		if !ok {
			continue
		}
		if strings.Contains(value, "${PACK}") {
			continue
		}
		if strings.Contains(value, "http://ongoing.invalid") {
			continue
		}
		t.Errorf("location %s is neither a ${PACK} path nor the unresolvable endpoint the "+
			"recorded catalog stands in for", value)
	}
	if !strings.Contains(text, `replay = "${PACK}/sources/projects"`) {
		t.Error("the project sources do not declare a recording, so the pack would reach the network")
	}
}
