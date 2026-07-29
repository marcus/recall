package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/recall"
)

// These tests are about the committed smoke pack rather than about this
// package's types: a pack is data, and data that nothing checks drifts. They
// assert the invariants a runner would otherwise discover one broken case at a
// time — that every judgment names a case, that every lineage root is in
// persisted form and names a source the pack actually configures, that every
// judged document chunk is a chunk the document adapter really produces from
// the committed corpus, and that each category docs/evaluation.md requires is
// present under a tag a report can group by.
//
// The pack cannot be executed here. `recall eval run` is built separately, so
// what is checkable without it is structure and fixture reality, and that is
// what these cover.

const smokePackRel = "../../eval/packs/smoke"

// smokeCaseBounds is the range docs/evaluation.md commits the smoke pack to:
// "30–50 synthetic smoke cases covering every category above".
const (
	smokeMinCases = 30
	smokeMaxCases = 50
)

func smokePackDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(smokePackRel)
	if err != nil {
		t.Fatalf("resolve pack dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("smoke pack is missing: %v", err)
	}
	return dir
}

// smokeLoad reads the pack through the ordinary loader, which schema-checks
// every record on the way in. Nothing here reimplements that parsing: a test
// with its own reader would pass on a pack the runner cannot read.
func smokeLoad(t *testing.T) (*eval.Pack, []eval.Case, []eval.Judgment) {
	t.Helper()
	pack, err := eval.LoadPack(smokePackDir(t))
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

func TestSmokePackLoadsAndValidates(t *testing.T) {
	t.Parallel()
	pack, cases, judgments := smokeLoad(t)

	if err := eval.Validate(pack, cases, judgments); err != nil {
		t.Fatalf("pack does not validate:\n%v", err)
	}
	if pack.NetworkAccess {
		t.Error("the smoke pack declares network access; only a live health pack may")
	}
	if pack.Sources == "" || pack.Transcripts == "" {
		t.Error("the pack must name its fixture and transcript directories, or a runner " +
			"has to guess where its evidence lives")
	}
	if pack.SensitivityCeiling == nil {
		t.Error("the pack must declare a sensitivity ceiling: the zero-violations gate has " +
			"nothing to measure against otherwise")
	}
	if n := len(cases); n < smokeMinCases || n > smokeMaxCases {
		t.Errorf("%d cases, want %d to %d", n, smokeMinCases, smokeMaxCases)
	}
	if len(judgments) == 0 {
		t.Error("no judgments: every ranking metric would be undefined for every case")
	}
}

func TestSmokePackCaseInvariants(t *testing.T) {
	t.Parallel()
	pack, cases, judgments := smokeLoad(t)

	judged := map[string]int{}
	for _, j := range judgments {
		judged[j.CaseID]++
	}

	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.CaseID] {
			t.Errorf("case %q: declared twice, which makes its judgments ambiguous", c.CaseID)
		}
		seen[c.CaseID] = true

		if strings.TrimSpace(c.Notes) == "" {
			t.Errorf("case %q: no notes; a case nobody can explain the purpose of is a case "+
				"nobody will maintain correctly", c.CaseID)
		}
		if len(c.Tags) == 0 {
			t.Errorf("case %q: no tags; metrics are reported per tag, so an untagged case is "+
				"invisible to every group report", c.CaseID)
		}
		if c.Profile == "" {
			t.Errorf("case %q: no profile", c.CaseID)
		}
		if !c.ExpectedBehavior.Valid() {
			t.Errorf("case %q: expected_behavior %q is not a defined behavior",
				c.CaseID, c.ExpectedBehavior)
		}
		// A case with neither judgments nor an abstention expectation states
		// nothing: it can neither be scored nor failed.
		if judged[c.CaseID] == 0 && c.ExpectedBehavior != eval.BehaviorAbstain {
			t.Errorf("case %q: no judgments and no abstention expectation; nothing about it "+
				"can pass or fail", c.CaseID)
		}
		if c.Assertions == nil {
			t.Errorf("case %q: no assertions block; policy metrics have no ground truth "+
				"for it", c.CaseID)
		}
	}

	for _, j := range judgments {
		if !seen[j.CaseID] {
			t.Errorf("judgment for %q: no such case", j.CaseID)
		}
	}
	if pack.Profile == "" {
		t.Error("the pack names no default profile")
	}
}

// TestSmokePackLineageRootsArePersisted checks the form judgments key on.
// Display form would resolve against whatever a source happens to be called
// today, which is the failure source_uid exists to prevent.
func TestSmokePackLineageRootsArePersisted(t *testing.T) {
	t.Parallel()
	_, cases, judgments := smokeLoad(t)

	check := func(where string, root recall.LineageRoot) {
		loc, err := root.Locator()
		if err != nil {
			t.Errorf("%s: %q is not a persisted-form locator: %v", where, root, err)
			return
		}
		if loc.SourceUID == "" || loc.Local == "" {
			t.Errorf("%s: %q is not <source_uid>:<local>", where, root)
		}
		if loc.SourceID != "" {
			t.Errorf("%s: %q carries a display name; persisted references key on the uid",
				where, root)
		}
	}
	for _, j := range judgments {
		check("judgment "+j.CaseID, j.LineageRoot)
	}
	for _, c := range cases {
		if c.Assertions == nil {
			continue
		}
		for root := range c.Assertions.ExpectedRevisions {
			check("case "+c.CaseID+" expected_revisions", root)
		}
		for _, root := range c.Assertions.SuppressedLineages {
			check("case "+c.CaseID+" suppressed_lineages", root)
		}
		for _, root := range c.Assertions.VisibleLineages {
			check("case "+c.CaseID+" visible_lineages", root)
		}
	}
}

// smokeRequiredTags maps each category docs/evaluation.md requires of the
// smoke pack onto the tag that carries it. Tags are how a pack declares what a
// case is testing, so they are the only place this coverage can be asserted —
// and the case-tag macro average is the dimension a regression is visible
// along.
var smokeRequiredTags = map[string]string{
	"exact identifiers":              "exact",
	"aliases":                        "alias",
	"near-miss false positives":      "near-miss",
	"lexical paraphrase":             "paraphrase",
	"vocabulary-mismatch paraphrase": "paraphrase-semantic",
	"cross-source fusion":            "cross-source",
	"duplicate lineage":              "duplicate-lineage",
	"distinct corroborating roots":   "corroboration",
	"current evidence":               "current",
	"historical evidence":            "historical",
	"stale evidence":                 "stale",
	"superseded evidence":            "superseded",
	"answer behavior":                "answer",
	"abstain behavior":               "abstain",
	"unavailable source":             "unavailable",
	"denied source":                  "denied",
	"partial source":                 "partial",
	"timed-out source":               "timeout",
	"expansion, summary":             "detail-summary",
	"expansion, excerpt":             "detail-excerpt",
	"expansion, full":                "detail-full",
	"expansion, context":             "detail-context",
	"locator revision":               "locator-revision",
	"as_of against as_of none":       "as-of-unsupported",
	"config trust boundary":          "config-trust",
	"suppression in pre_reply":       "suppress-pre-reply",
	"suppression in explicit":        "suppress-explicit",
	"sensitivity ceiling":            "sensitivity-ceiling",
}

func TestSmokePackCoversEveryCategory(t *testing.T) {
	t.Parallel()
	_, cases, _ := smokeLoad(t)

	present := map[string][]string{}
	for _, c := range cases {
		for _, tag := range c.Tags {
			present[tag] = append(present[tag], c.CaseID)
		}
	}
	categories := make([]string, 0, len(smokeRequiredTags))
	for name := range smokeRequiredTags {
		categories = append(categories, name)
	}
	sort.Strings(categories)

	for _, name := range categories {
		if len(present[smokeRequiredTags[name]]) == 0 {
			t.Errorf("no case tagged %q: the pack does not cover %s",
				smokeRequiredTags[name], name)
		}
	}
}

// TestSmokePackAbstentionsAreDeliberate keeps the abstention cases from
// becoming a place where a case with nothing to say hides. An abstention is a
// claim about the corpus, so it has to be stated, not defaulted into.
func TestSmokePackAbstentionsAreDeliberate(t *testing.T) {
	t.Parallel()
	_, cases, judgments := smokeLoad(t)

	required := map[string]bool{}
	for _, j := range judgments {
		if j.Required {
			required[j.CaseID] = true
		}
	}
	abstentions := 0
	for _, c := range cases {
		if c.ExpectedBehavior != eval.BehaviorAbstain {
			continue
		}
		abstentions++
		if required[c.CaseID] {
			t.Errorf("case %q expects an abstention but marks evidence required; a case "+
				"cannot both have nothing to find and something that must be found", c.CaseID)
		}
		if c.Assertions == nil || c.Assertions.ExpectedCoverage == "" {
			t.Errorf("case %q abstains without stating expected coverage; outcome and "+
				"coverage are orthogonal and both have to be graded", c.CaseID)
		}
	}
	if abstentions == 0 {
		t.Error("no abstention cases: abstention accuracy would be undefined for the pack")
	}
}

// smokeSemanticTag marks the paraphrase cases whose answer shares almost no
// surface term with the question. They are the pack's measurement of the
// vocabulary-mismatch gap, so the property that makes them measure it — the
// overlap — is asserted here rather than trusted to a note.
const smokeSemanticTag = "paraphrase-semantic"

// smokeFunctionWords mirrors internal/adapters/docs.isEnglishFunctionWord.
//
// It is duplicated rather than exported because this test is an audit of the
// pack, not of the adapter: what it needs is the same reading of "content term"
// a person would do by hand when writing one of these cases. If the adapter's
// list grows, the audit here becomes conservative — it counts a word the
// adapter would have dropped — which can only make the bound harder to satisfy.
var smokeFunctionWords = map[string]bool{
	"a": true, "an": true, "the": true,
	"am": true, "are": true, "be": true, "been": true, "being": true, "did": true,
	"do": true, "does": true, "is": true, "was": true, "were": true,
	"what": true, "when": true, "where": true, "which": true, "who": true,
	"whom": true, "whose": true, "why": true, "how": true,
}

var smokeWordPattern = regexp.MustCompile(`[0-9A-Za-z]+`)

func smokeContentTerms(s string) []string {
	var out []string
	for _, tok := range smokeWordPattern.FindAllString(strings.ToLower(s), -1) {
		if !smokeFunctionWords[tok] {
			out = append(out, tok)
		}
	}
	return out
}

// smokeChunkText returns the text a judged document chunk is indexed under: its
// own lines, plus the document's H1, which the chunker carries into every
// section's term set as the ancestor heading path.
func smokeChunkText(t *testing.T, corpus, local string) string {
	t.Helper()
	path, span, ok := strings.Cut(local, "#")
	if !ok {
		t.Fatalf("%q is not a chunk locator", local)
	}
	var first, last int
	if _, err := fmt.Sscanf(span, "L%d-L%d", &first, &last); err != nil {
		t.Fatalf("%q does not carry a line span: %v", local, err)
	}
	raw, err := os.ReadFile(filepath.Join(smokePackDir(t), "sources", corpus, path))
	if err != nil {
		t.Fatalf("read judged fixture: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	if last > len(lines) {
		t.Fatalf("%q names line %d of a %d-line file", local, last, len(lines))
	}
	text := strings.Join(lines[first-1:last], "\n")
	if strings.HasPrefix(lines[0], "# ") && first > 1 {
		text = lines[0] + "\n" + text
	}
	return text
}

// TestSmokePackParaphraseSemanticOverlap is the audit that makes the
// vocabulary-mismatch family mean what it says.
//
// A case in this family claims that its answer cannot be reached by matching
// words, and the number that claim produces is a near-zero one. Nothing stops a
// later edit from putting one of the question's words into the answering passage
// — at which point the case starts being answerable lexically, its score rises,
// and the rise looks like a semantic source earning something it did not. So the
// overlap is bounded here: at most one content term shared between the question
// and the passage judged authoritative for it.
func TestSmokePackParaphraseSemanticOverlap(t *testing.T) {
	t.Parallel()
	_, cases, judgments := smokeLoad(t)

	corpora := map[recall.SourceUID]string{
		"01SMOKENOTES":    "notes",
		"01SMOKEHANDBOOK": "handbook",
	}
	requiredRoots := map[string][]recall.LineageRoot{}
	judged := map[string]int{}
	for _, j := range judgments {
		judged[j.CaseID]++
		if j.Required {
			requiredRoots[j.CaseID] = append(requiredRoots[j.CaseID], j.LineageRoot)
		}
	}

	answering, abstaining := 0, 0
	for _, c := range cases {
		semantic := false
		paraphrase := false
		for _, tag := range c.Tags {
			switch tag {
			case smokeSemanticTag:
				semantic = true
			case "paraphrase":
				paraphrase = true
			}
		}
		if !semantic {
			continue
		}
		if !paraphrase {
			t.Errorf("case %q is tagged %q but not paraphrase; the family has to be visible "+
				"in the group the pack already reports, or its regression hides in a tag "+
				"nothing else compares", c.CaseID, smokeSemanticTag)
		}
		if c.ExpectedBehavior == eval.BehaviorAbstain {
			abstaining++
			if judged[c.CaseID] != 0 {
				t.Errorf("case %q must abstain but carries %d judgment(s); an undefined "+
					"ranking metric is not a zero, and grading a question with no answer "+
					"would score honesty as a ranking failure",
					c.CaseID, judged[c.CaseID])
			}
			continue
		}
		answering++

		roots := requiredRoots[c.CaseID]
		if len(roots) != 1 {
			t.Errorf("case %q names %d required lineage roots; a vocabulary-mismatch case "+
				"is about one passage the words cannot reach", c.CaseID, len(roots))
			continue
		}
		if c.Assertions == nil || c.Assertions.ExpectedTopLineage == "" {
			t.Errorf("case %q declares no expected_top_lineage; the whole claim is which "+
				"single passage answers, and no graded metric can state it", c.CaseID)
		} else if c.Assertions.ExpectedTopLineage != roots[0] {
			t.Errorf("case %q must rank %q first but marks %q required",
				c.CaseID, c.Assertions.ExpectedTopLineage, roots[0])
		}

		loc, err := roots[0].Locator()
		if err != nil {
			t.Errorf("case %q: %v", c.CaseID, err)
			continue
		}
		corpus, ok := corpora[loc.SourceUID]
		if !ok {
			t.Errorf("case %q is judged on source %q, which this audit cannot read as a "+
				"document corpus", c.CaseID, loc.SourceUID)
			continue
		}

		passage := map[string]bool{}
		for _, term := range smokeContentTerms(smokeChunkText(t, corpus, loc.Local)) {
			passage[term] = true
		}
		var shared []string
		for _, term := range smokeContentTerms(c.Query) {
			if passage[term] {
				shared = append(shared, term)
			}
		}
		sort.Strings(shared)
		if len(shared) > 1 {
			t.Errorf("case %q shares %d content terms with the passage judged authoritative "+
				"for it (%v); this family measures the questions words cannot answer, so at "+
				"most one is allowed", c.CaseID, len(shared), shared)
		}
	}

	if answering < 3 {
		t.Errorf("%d answering %s cases; one vocabulary gap is an anecdote, a family of "+
			"them is a metric", answering, smokeSemanticTag)
	}
	if abstaining == 0 {
		t.Errorf("no must-abstain %s case: a semantic source would be measured on recall "+
			"and never on honesty", smokeSemanticTag)
	}
}

// --------------------------------------------------------------------------
// Fixture reality: the pack's sources must be things the adapters can read.
// --------------------------------------------------------------------------

var (
	smokeUIDPattern     = regexp.MustCompile(`(?m)^source_uid\s*=\s*"([^"]+)"`)
	smokeIDPattern      = regexp.MustCompile(`(?m)^source_id\s*=\s*"([^"]+)"`)
	smokeProfilePattern = regexp.MustCompile(`(?m)^\[profiles\.([^\]]+)\]`)
	smokeLocPattern     = regexp.MustCompile(`(?m)^location\s*=\s*"([^"]+)"`)
)

func smokeConfigText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(smokePackDir(t), "sources", "config.toml"))
	if err != nil {
		t.Fatalf("read pack configuration: %v", err)
	}
	return string(raw)
}

func smokeSet(matches [][]string) map[string]bool {
	out := map[string]bool{}
	for _, m := range matches {
		out[m[1]] = true
	}
	return out
}

// TestSmokePackNamesOnlyConfiguredSources ties the judgments to the pack's own
// configuration. A judgment keyed on a uid nothing declares never fires: the
// metric quietly loses its ground truth instead of failing.
func TestSmokePackNamesOnlyConfiguredSources(t *testing.T) {
	t.Parallel()
	_, cases, judgments := smokeLoad(t)
	text := smokeConfigText(t)

	uids := smokeSet(smokeUIDPattern.FindAllStringSubmatch(text, -1))
	profiles := smokeSet(smokeProfilePattern.FindAllStringSubmatch(text, -1))

	uidOf := func(root recall.LineageRoot) string {
		loc, err := root.Locator()
		if err != nil {
			return ""
		}
		return string(loc.SourceUID)
	}
	for _, j := range judgments {
		if uid := uidOf(j.LineageRoot); uid != "" && !uids[uid] {
			t.Errorf("judgment %q names source_uid %q, which the pack does not configure",
				j.CaseID, uid)
		}
	}
	for _, c := range cases {
		if !profiles[c.Profile] {
			t.Errorf("case %q names profile %q, which the pack does not configure",
				c.CaseID, c.Profile)
		}
		if c.Assertions == nil {
			continue
		}
		for uid := range c.Assertions.ExpectedSourceOutcomes {
			if !uids[string(uid)] {
				t.Errorf("case %q asserts an outcome for unconfigured source %q", c.CaseID, uid)
			}
		}
		for _, uid := range append(append([]recall.SourceUID(nil),
			c.Assertions.RequiredSources...), c.Assertions.ForbiddenSources...) {
			if !uids[string(uid)] {
				t.Errorf("case %q names unconfigured source %q", c.CaseID, uid)
			}
		}
	}
}

// TestSmokePackConfigIsPortable holds the one rule that lets a pack of fixture
// sources be committed at all: no machine paths. Locations are written with a
// placeholder the runner binds, the same convention the adapter conformance
// transcripts already use.
func TestSmokePackConfigIsPortable(t *testing.T) {
	t.Parallel()
	text := smokeConfigText(t)

	locations := smokeLocPattern.FindAllStringSubmatch(text, -1)
	if len(locations) == 0 {
		t.Fatal("the pack configuration declares no source locations")
	}
	for _, m := range locations {
		if !strings.Contains(m[1], "${PACK}") {
			t.Errorf("location %q is not written against ${PACK}; a committed pack cannot "+
				"contain a machine path", m[1])
		}
	}
	if ids := smokeSet(smokeIDPattern.FindAllStringSubmatch(text, -1)); len(ids) < 2 {
		t.Error("the pack configures fewer than two sources; cross-source fusion cannot be " +
			"exercised by one")
	}
}

// smokeJudgedChunks collects the document locals the judgments claim exist,
// grouped by the corpus directory that must contain them.
func smokeJudgedChunks(t *testing.T, judgments []eval.Judgment) map[string]map[string]bool {
	t.Helper()
	corpora := map[string]string{
		"01SMOKENOTES":    "notes",
		"01SMOKEHANDBOOK": "handbook",
		"01SMOKEVAULT":    "vault",
		"01SMOKELEDGER":   "ledger",
	}
	out := map[string]map[string]bool{}
	for _, j := range judgments {
		loc, err := j.LineageRoot.Locator()
		if err != nil {
			continue
		}
		dir, ok := corpora[string(loc.SourceUID)]
		if !ok {
			continue
		}
		if out[dir] == nil {
			out[dir] = map[string]bool{}
		}
		out[dir][loc.Local] = true
	}
	return out
}

// TestSmokePackDocumentFixturesIndex builds the real document adapter over the
// committed corpora and checks that every judged chunk locator is one the
// adapter actually produces.
//
// This is the check a hand-written pack most needs. A chunk locator carries a
// line range, so a judgment can be perfectly well-formed, name a real file, and
// still refer to a range no chunk has — and every ranking metric for that case
// would then be measured against evidence the system can never return.
func TestSmokePackDocumentFixturesIndex(t *testing.T) {
	t.Parallel()
	_, _, judgments := smokeLoad(t)
	pack := smokePackDir(t)

	settings := map[string]map[string]any{
		"notes": {
			"extensions": []any{".md"},
			"aliases": map[string]any{
				"projects/recall/decisions.md": []any{"ADR log"},
				"runbooks/backup-restore.md":   []any{"restore drill"},
			},
		},
		"handbook": {"extensions": []any{".md"}},
		"vault":    {"extensions": []any{".md"}},
		"ledger":   {"extensions": []any{".md"}},
	}

	for corpus, wanted := range smokeJudgedChunks(t, judgments) {
		t.Run(corpus, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			a := docs.New()
			t.Cleanup(func() { _ = a.Close() })

			if _, err := a.Initialize(ctx, adapter.Config{
				ProtocolVersionMin: 1,
				ProtocolVersionMax: 1,
				Workdir:            t.TempDir(),
				SourceID:           corpus,
				Location:           filepath.Join(pack, "sources", corpus),
				Settings:           settings[corpus],
			}); err != nil {
				t.Fatalf("the %s corpus is not indexable: %v", corpus, err)
			}

			// Search each judged document by its own path: a path is an exact
			// identifier, so the whole document comes back and every chunk of
			// it is visible in one call.
			found := map[string]bool{}
			for local := range wanted {
				path, _, _ := strings.Cut(local, "#")
				resp, err := a.Search(ctx, recall.SearchRequest{
					Query:    path,
					Limit:    200,
					Deadline: time.Now().Add(10 * time.Second),
				})
				if err != nil {
					t.Fatalf("search %q: %v", path, err)
				}
				for _, c := range resp.Candidates {
					found[c.Locator.Local] = true
				}
			}
			for local := range wanted {
				if !found[local] {
					t.Errorf("judged chunk %q is not a chunk the adapter produces from the "+
						"%s corpus; the judgment can never match a candidate", local, corpus)
				}
			}
		})
	}
}

// smokeTasksRunner replays the recorded Tasks CLI output in the pack.
//
// It is the same seam a runner uses: the adapter takes a Runner, so a hermetic
// evaluation costs no process spawn and the pack ships no executable. The
// matching rules are the ones sources/tasks/cli.json declares.
type smokeTasksRunner struct {
	dir   string
	rules []smokeCLIRule
	def   smokeCLIRule
}

type smokeCLIRule struct {
	Args     []string `json:"args"`
	Contains []string `json:"contains"`
	Stdout   string   `json:"stdout"`
	ExitCode int      `json:"exit_code"`
}

func (r smokeCLIRule) matches(args []string) bool {
	if len(r.Args) > 0 {
		return slicesEqualSmoke(r.Args, args)
	}
	for _, want := range r.Contains {
		found := false
		for _, got := range args {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(r.Contains) > 0
}

func slicesEqualSmoke(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newSmokeTasksRunner(t *testing.T, dir string) *smokeTasksRunner {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "cli.json"))
	if err != nil {
		t.Fatalf("read recorded CLI map: %v", err)
	}
	var doc struct {
		Invocations []smokeCLIRule `json:"invocations"`
		Default     smokeCLIRule   `json:"default"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse recorded CLI map: %v", err)
	}
	if len(doc.Invocations) == 0 {
		t.Fatal("the recorded CLI map declares no invocations")
	}
	return &smokeTasksRunner{dir: dir, rules: doc.Invocations, def: doc.Default}
}

func (r *smokeTasksRunner) Run(ctx context.Context, args ...string) (tasks.Result, error) {
	if err := ctx.Err(); err != nil {
		return tasks.Result{}, err
	}
	rule := r.def
	for _, candidate := range r.rules {
		if candidate.matches(args) {
			rule = candidate
			break
		}
	}
	out, err := os.ReadFile(filepath.Join(r.dir, rule.Stdout))
	if err != nil {
		return tasks.Result{}, err
	}
	return tasks.Result{Stdout: out, ExitCode: rule.ExitCode, Elapsed: time.Millisecond}, nil
}

// TestSmokePackTaskFixturesResolve runs the judged task ids through the real
// Tasks adapter over the recorded CLI output.
//
// Same reason as the document check: a judgment naming a task the fixture store
// does not hold is well-formed, unfalsifiable, and silently removes its case's
// ground truth.
func TestSmokePackTaskFixturesResolve(t *testing.T) {
	t.Parallel()
	_, _, judgments := smokeLoad(t)

	wanted := map[string]bool{}
	for _, j := range judgments {
		loc, err := j.LineageRoot.Locator()
		if err == nil && loc.SourceUID == "01SMOKETASKS" {
			wanted[loc.Local] = true
		}
	}
	if len(wanted) == 0 {
		t.Fatal("no judgment names the Tasks source; the exact-identifier cases have no " +
			"structured source to be about")
	}

	ctx := context.Background()
	dir := filepath.Join(smokePackDir(t), "sources", "tasks")
	a := tasks.New(tasks.Options{Runner: newSmokeTasksRunner(t, dir)})
	t.Cleanup(func() { _ = a.Close() })

	if _, err := a.Initialize(ctx, adapter.Config{
		ProtocolVersionMin: 1,
		ProtocolVersionMax: 1,
		Workdir:            t.TempDir(),
		SourceID:           "tasks",
		Location:           dir,
	}); err != nil {
		t.Fatalf("initialize tasks adapter: %v", err)
	}

	for id := range wanted {
		resp, err := a.Search(ctx, recall.SearchRequest{
			Query:    id,
			Limit:    20,
			Deadline: time.Now().Add(10 * time.Second),
		})
		if err != nil {
			t.Fatalf("search %q: %v", id, err)
		}
		found := false
		for _, c := range resp.Candidates {
			if c.Locator.Local == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("judged task %q is not in the recorded store; the judgment can never "+
				"match a candidate", id)
		}
	}
}

// smokeStreamLines reads a JSONL fixture and reports how many lines parsed and
// how many did not.
func smokeStreamLines(t *testing.T, path string) (parsed, broken, unknownSchema int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Schema int    `json:"schema"`
			ID     string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			broken++
			continue
		}
		if rec.ID == "" {
			t.Errorf("%s: a record with no id cannot be located again", filepath.Base(path))
		}
		if rec.Schema < 1 || rec.Schema > 2 {
			unknownSchema++
		}
		parsed++
	}
	return parsed, broken, unknownSchema
}

// TestSmokePackStreamFixtures checks that the stream corpora are what the
// cases claim: readable where a case expects success, and damaged in exactly
// the ways a case expects to be reported.
func TestSmokePackStreamFixtures(t *testing.T) {
	t.Parallel()
	sources := filepath.Join(smokePackDir(t), "sources")

	for _, name := range []string{
		filepath.Join("signals", "signals.jsonl"),
		filepath.Join("signals", "signals-archive.jsonl"),
		filepath.Join("slow", "signals.jsonl"),
	} {
		parsed, broken, unknown := smokeStreamLines(t, filepath.Join(sources, name))
		if parsed == 0 {
			t.Errorf("%s holds no records", name)
		}
		if broken != 0 || unknown != 0 {
			t.Errorf("%s: %d unreadable and %d unparseable-version records; this fixture "+
				"backs cases that expect a clean success", name, broken, unknown)
		}
	}

	// The partial fixture earns its outcome from real damage, not from a flag.
	parsed, broken, unknown := smokeStreamLines(t, filepath.Join(sources, "flaky", "signals.jsonl"))
	switch {
	case broken == 0:
		t.Error("the flaky fixture has no truncated record, so nothing makes the source partial")
	case unknown == 0:
		t.Error("the flaky fixture has no record from a newer producer, so only one kind of " +
			"partial coverage is exercised")
	case parsed == unknown:
		t.Error("the flaky fixture has nothing readable; a partial source still answers")
	}
	if _, err := os.Stat(filepath.Join(sources, "flaky", "signals-archive.jsonl")); !os.IsNotExist(err) {
		t.Error("the flaky fixture's archive file exists; the case expects a configured file " +
			"that is absent, which is missing coverage rather than an empty archive")
	}

	// The unavailable fixture must be missing the file it is configured to read
	// and must not be an empty directory, which would be a different failure.
	if _, err := os.Stat(filepath.Join(sources, "rotated", "signals.jsonl")); !os.IsNotExist(err) {
		t.Error("the rotated fixture still holds signals.jsonl; the case expects the " +
			"configured file to be gone")
	}
	if entries, err := os.ReadDir(filepath.Join(sources, "rotated")); err != nil || len(entries) == 0 {
		t.Error("the rotated fixture is empty; a rotated stream leaves its previous " +
			"generation behind under another name")
	}
}

// TestSmokePackTranscripts checks the replayed transcripts against the format
// the repository already defines for conformance recordings.
func TestSmokePackTranscripts(t *testing.T) {
	t.Parallel()
	root := filepath.Join(smokePackDir(t), "transcripts")

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read transcripts: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no transcripts: the denied-source case has nothing to replay")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(root, e.Name())

			raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			var m struct {
				Case        string `json:"case"`
				Description string `json:"description"`
				Flow        string `json:"flow"`
				Responses   int    `json:"responses"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("parse manifest: %v", err)
			}
			if m.Case != e.Name() {
				t.Errorf("manifest names case %q in directory %q", m.Case, e.Name())
			}
			if strings.TrimSpace(m.Description) == "" {
				t.Error("a transcript nobody can read is not documentation")
			}
			if m.Flow != "lockstep" {
				t.Errorf("flow %q is not a defined flow", m.Flow)
			}

			for _, file := range []string{"request.jsonl", "response.jsonl"} {
				lines := smokeJSONLines(t, filepath.Join(dir, file))
				if len(lines) == 0 {
					t.Errorf("%s is empty", file)
				}
				if file == "response.jsonl" && len(lines) != m.Responses {
					t.Errorf("%s holds %d frames, manifest declares %d; a case that silently "+
						"stops answering must fail rather than pass with a short list",
						file, len(lines), m.Responses)
				}
			}
		})
	}
}

func smokeJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []map[string]any
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("%s:%d: %v", filepath.Base(path), i+1, err)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Errorf("%s:%d: not a JSON-RPC 2.0 frame", filepath.Base(path), i+1)
		}
		out = append(out, frame)
	}
	return out
}

// TestSmokePackProjectFixturesAreRejected runs the trust-boundary fixtures
// through the real configuration loader.
//
// The pack's assertion vocabulary cannot say "no subprocess was spawned" — that
// is a run-level gate — but it can say that the fixture is genuinely
// dangerous. A project file that quietly loaded would make the config-trust
// cases pass while testing nothing.
func TestSmokePackProjectFixturesAreRejected(t *testing.T) {
	t.Parallel()
	pack := smokePackDir(t)

	for _, name := range []string{"project-command", "project-settings-command"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "recall"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A minimal trusted layer, so the only thing under test is the
			// project file.
			user := `[defaults]
profile = "smoke"
timeout_ms = 1000

[[sources]]
source_uid = "01SMOKENOTES"
source_id = "notes"
adapter = "documents"
location = "/dev/null"
freshness_mode = "indexed"
sensitivity = "internal"
base_prior = 1.0
timeout_ms = 400

[profiles.smoke]
sources = ["notes"]
max_sensitivity = "internal"
`
			if err := os.WriteFile(filepath.Join(home, "recall", "config.toml"),
				[]byte(user), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := config.Load(config.Options{
				Paths: config.Paths{
					ConfigHome: home,
					StateHome:  filepath.Join(home, "state"),
					CacheHome:  filepath.Join(home, "cache"),
				},
				ProjectFile: filepath.Join(pack, "sources", name, "recall.toml"),
				Builtins: []config.Builtin{{
					Name:           "documents",
					FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
				}},
			})
			if err == nil {
				t.Fatalf("%s loaded; the fixture is supposed to be refused, and a fixture "+
					"that loads makes its case vacuous", name)
			}
			if !strings.Contains(err.Error(), "command") {
				t.Errorf("%s was refused, but not for its executable key: %v", name, err)
			}
		})
	}
}
