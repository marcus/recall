package ranking_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/pkg/recall"
)

var resolver = lineage.MapResolver{
	"tasks":   "uid-tasks",
	"signals": "uid-signals",
	"docs":    "uid-docs",
	"mail":    "uid-mail",
}

// newRanker builds a ranker whose sources all carry the neutral prior, so any
// score difference a test observes comes from rank and lineage rather than
// from configuration nobody wrote down.
func newRanker(t *testing.T, tweak func(*ranking.Config)) *ranking.Ranker {
	t.Helper()
	cfg := ranking.Config{
		Sources: map[recall.SourceUID]ranking.SourceConfig{
			"uid-tasks":   {SourceID: "tasks", BasePrior: 1},
			"uid-signals": {SourceID: "signals", BasePrior: 1},
			"uid-docs":    {SourceID: "docs", BasePrior: 1},
			"uid-mail":    {SourceID: "mail", BasePrior: 1},
		},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	r, err := ranking.New(cfg)
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	return r
}

type opt func(*recall.Candidate)

func cand(sourceID, local string, rank int, opts ...opt) recall.Candidate {
	uid, ok := resolver.UID(sourceID)
	if !ok {
		panic("unknown source in test: " + sourceID)
	}
	c := recall.Candidate{
		CandidateID:  sourceID + "/" + local,
		SourceUID:    uid,
		SourceID:     sourceID,
		Locator:      recall.Locator{SourceID: sourceID, SourceUID: uid, Local: local},
		LocalRank:    rank,
		RecordType:   recall.RecordDocument,
		MatchSignals: []recall.MatchSignal{recall.MatchLexical},
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func from(parents ...string) opt {
	return func(c *recall.Candidate) {
		for _, p := range parents {
			loc, err := recall.ParseLocator(p)
			if err != nil {
				panic(err)
			}
			c.DerivedFrom = append(c.DerivedFrom, loc)
		}
	}
}

func title(s string) opt           { return func(c *recall.Candidate) { c.Title = s } }
func kind(t recall.RecordType) opt { return func(c *recall.Candidate) { c.RecordType = t } }
func recordID(s string) opt        { return func(c *recall.Candidate) { c.SourceRecordID = s } }
func fingerprint(s string) opt     { return func(c *recall.Candidate) { c.ContentFingerprint = s } }
func revision(s string) opt        { return func(c *recall.Candidate) { c.SourceRevision = s } }
func excerptKind(k recall.ExcerptKind) opt {
	return func(c *recall.Candidate) { c.ExcerptKind = k }
}

func exact() opt {
	return func(c *recall.Candidate) {
		c.MatchSignals = append(c.MatchSignals, recall.MatchExactIdentifier)
	}
}

func meta(k string, v any) opt {
	return func(c *recall.Candidate) {
		if c.Metadata == nil {
			c.Metadata = map[string]any{}
		}
		c.Metadata[k] = v
	}
}

// score sets the source's native relevance number. Nothing in fusion may read
// it; tests set it precisely to prove that.
func score(v float64) opt { return func(c *recall.Candidate) { c.LocalScore = &v } }

func request(cands ...recall.Candidate) ranking.Request {
	return ranking.Request{Candidates: cands, Resolver: resolver}
}

func fuse(t *testing.T, r *ranking.Ranker, req ranking.Request) ranking.Fusion {
	t.Helper()
	out, err := r.Fuse(req)
	if err != nil {
		t.Fatalf("Fuse: %v", err)
	}
	return out
}

// order is the part of a fusion a caller actually sees ordered: which record
// won, and what it scored.
func order(f ranking.Fusion) []string {
	out := make([]string, 0, len(f.Results))
	for _, res := range f.Results {
		out = append(out, res.Primary.Locator.String())
	}
	return out
}

func single(t *testing.T, f ranking.Fusion) recall.Result {
	t.Helper()
	if len(f.Results) != 1 {
		t.Fatalf("results = %v, want exactly one", order(f))
	}
	return f.Results[0]
}

// The load-bearing invariant of lineage grouping. A record retrieved twice is
// one piece of evidence, so a duplicate must move the score by exactly zero —
// not by a little. "Close enough" here would mean every source mirroring
// another quietly reweights the whole ranking.
func TestDuplicateLineageScoresExactlyAsSingleSource(t *testing.T) {
	r := newRanker(t, nil)
	alone := single(t, fuse(t, r, request(cand("tasks", "td-1", 1))))

	duplicates := map[string]recall.Candidate{
		// Same evidence, same standing: the projection ranked first too.
		"equal rank":  cand("signals", "sig-1", 1, from("tasks:td-1")),
		"worse rank":  cand("signals", "sig-1", 9, from("tasks:td-1")),
		"third party": cand("docs", "note.md#2", 4, from("tasks:td-1")),
	}
	for name, dup := range duplicates {
		t.Run(name, func(t *testing.T) {
			got := single(t, fuse(t, r, request(cand("tasks", "td-1", 1), dup)))
			if got.Score != alone.Score {
				t.Errorf("score = %v, want exactly %v", got.Score, alone.Score)
			}
			if n := got.Explanation.Corroboration.IndependentUnits; n != 1 {
				t.Errorf("distinct lineages = %d, want 1", n)
			}
			if len(got.Members) != 1 {
				t.Errorf("members = %d, want one lineage group", len(got.Members))
			}
			// The duplicate is still reachable: it merged for scoring, not for
			// expansion.
			if n := len(got.Members[0].Candidates); n != 2 {
				t.Errorf("candidates in group = %d, want both views", n)
			}
		})
	}
}

// Two distinct records agreeing is real corroboration; ten are not ten times
// the evidence. The cap is what keeps a chatty source from manufacturing
// consensus with itself.
func TestIndependentUnitsCorroborateUpToTheCap(t *testing.T) {
	r := newRanker(t, nil)
	person := []opt{kind(recall.RecordPerson), meta(ranking.MetaEntityID, "person-42")}

	one := single(t, fuse(t, r, request(cand("tasks", "td-1", 1, person...))))

	two := single(t, fuse(t, r, request(
		cand("tasks", "td-1", 1, person...),
		cand("docs", "team.md#1", 1, person...),
	)))
	if want := 2 * one.Score; two.Score != want {
		t.Errorf("two lineages score %v, want the sum %v", two.Score, want)
	}
	if two.Explanation.Corroboration.CapApplied {
		t.Error("cap reported as applied at exactly the cap")
	}

	three := single(t, fuse(t, r, request(
		cand("tasks", "td-1", 1, person...),
		cand("docs", "team.md#1", 1, person...),
		cand("mail", "msg-7", 1, person...),
	)))
	if want := ranking.DefaultCorroborationCap * one.Score; three.Score != want {
		t.Errorf("three lineages score %v, want the clamped %v", three.Score, want)
	}
	corr := three.Explanation.Corroboration
	if !corr.CapApplied {
		t.Error("cap applied but not reported")
	}
	if corr.IndependentUnits != 3 {
		t.Errorf("distinct lineages = %d, want 3", corr.IndependentUnits)
	}
	if corr.Cap != ranking.DefaultCorroborationCap {
		t.Errorf("cap = %v, want the configured %v", corr.Cap, ranking.DefaultCorroborationCap)
	}
	if want := []string{"docs", "mail", "tasks"}; !reflect.DeepEqual(corr.Sources, want) {
		t.Errorf("contributing sources = %v, want %v", corr.Sources, want)
	}
}

// Adapters answer concurrently, so the pool arrives in whatever order the
// network produced. Two runs of the same query must still agree exactly, or no
// evaluation result means anything.
func TestOrderingIsDeterministicUnderShuffle(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.MaxPerSource = 2; c.Limit = 5 })

	// Every merge rule, both suppression reasons, and the diversity policy are
	// represented, so the assertion covers the order of the withheld list too.
	pool := []recall.Candidate{
		cand("tasks", "td-1", 1, title("Ship the adapter protocol")),
		cand("tasks", "td-2", 2, title("Ship the adapter protocol")),
		cand("tasks", "td-3", 3, kind(recall.RecordPerson), meta(ranking.MetaEntityID, "person-42")),
		cand("tasks", "td-4", 4, title("Unrelated errand")),
		cand("tasks", "td-5", 5, title("Yet another errand")),
		cand("signals", "sig-1", 1, from("tasks:td-1")),
		cand("signals", "sig-2", 2, kind(recall.RecordPerson), meta(ranking.MetaEntityID, "person-42")),
		cand("docs", "spec.md#1", 1, recordID("spec.md")),
		cand("docs", "spec.md#4", 2, recordID("spec.md")),
		cand("docs", "notes.md#1", 3, fingerprint("fp-aaa")),
		cand("mail", "msg-1", 1, fingerprint("fp-aaa")),
		cand("mail", "msg-2", 2, exact(), title("Recall weekly review")),
		cand("mail", "msg-3", 3, kind(recall.RecordTask), fingerprint("fp-aaa")),
		cand("mail", "msg-4", 4, title("Quarterly planning offsite")),
	}

	want := fuse(t, r, request(pool...))
	if len(want.Results) < 2 || len(want.Suppressed) < 2 {
		t.Fatalf("fixture produced %v and %+v; the test needs both to compare",
			order(want), want.Suppressed)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 64 {
		shuffled := append([]recall.Candidate(nil), pool...)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		got := fuse(t, r, request(shuffled...))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d changed the fusion:\n got %v\nwant %v", i, order(got), order(want))
		}
	}
}

// Exact promotion is a partition, not a bonus. A weak exact hit outranks a
// strongly corroborated lexical cluster, and it does so without its score being
// touched — so the score still means what it says.
func TestExactPromotionBeatsHigherScoringCluster(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.Sources["uid-mail"] = ranking.SourceConfig{SourceID: "mail", BasePrior: ranking.MinPrior}
	})
	person := []opt{kind(recall.RecordPerson), meta(ranking.MetaEntityID, "person-42")}

	got := fuse(t, r, request(
		cand("tasks", "td-1", 1, person...),
		cand("docs", "team.md#1", 1, person...),
		cand("mail", "msg-9", 40, exact()),
		cand("mail", "msg-8", 2, exact()),
	))
	// Promotion partitions; inside the partition, score still decides.
	want := []string{"mail:msg-8", "mail:msg-9", "docs:team.md#1"}
	if !reflect.DeepEqual(order(got), want) {
		t.Fatalf("order = %v, want %v", order(got), want)
	}

	promoted, outranked := got.Results[1], got.Results[2]
	if promoted.Primary.Locator.String() != "mail:msg-9" {
		t.Fatalf("first result = %s, want the exact match", promoted.Primary.Locator)
	}
	if !promoted.Explanation.ExactPromoted {
		t.Error("promotion not recorded in the explanation")
	}
	if promoted.Score >= outranked.Score {
		t.Errorf("promoted score %v >= outranked %v: promotion must not add to the score",
			promoted.Score, outranked.Score)
	}
	if outranked.Explanation.ExactPromoted {
		t.Error("non-exact cluster reported as promoted")
	}
}

// Exact is evidence an adapter recognized a stable name. It is not by itself
// evidence that a sentence is an identifier lookup: a project can be the
// subject of a question. Natural-language intent keeps the signal but disables
// only the partition, so ordinary relevance can put the answer first.
func TestNaturalLanguageExactMatchKeepsSignalWithoutPartitioning(t *testing.T) {
	r := newRanker(t, nil)
	req := request(
		cand("mail", "project-health", 1, exact(), relevance(0.1)),
		cand("docs", "answer.md#1", 2, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassNaturalLanguage

	got := fuse(t, r, req)
	if want := []string{"docs:answer.md#1", "mail:project-health"}; !reflect.DeepEqual(order(got), want) {
		t.Fatalf("order = %v, want %v", order(got), want)
	}
	health := got.Results[1]
	if !health.Primary.HasSignal(recall.MatchExactIdentifier) {
		t.Fatal("natural-language result lost its exact_identifier signal")
	}
	if health.Explanation.ExactPromoted {
		t.Fatal("natural-language exact match reported a partition that did not apply")
	}
}

func TestNaturalLanguageDeclaredAliasStillPartitionsItsCandidate(t *testing.T) {
	r := newRanker(t, nil)
	req := request(
		cand("mail", "declared-alias", 20, exact(), func(c *recall.Candidate) {
			c.MatchSignals = append(c.MatchSignals, recall.MatchAlias)
		}, relevance(0.1)),
		cand("docs", "answer.md#1", 1, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassNaturalLanguage

	got := fuse(t, r, req)
	if got.Results[0].Primary.Locator.String() != "mail:declared-alias" {
		t.Fatalf("order = %v, want the candidate-specific declared alias first", order(got))
	}
	if !got.Results[0].Explanation.ExactPromoted {
		t.Fatal("declared alias did not report exact promotion")
	}
}

func TestNaturalLanguageClusterCannotAssembleExactAliasAcrossCandidates(t *testing.T) {
	r := newRanker(t, nil)
	subject := []opt{meta(ranking.MetaEntityID, "subject-1")}
	req := request(
		cand("mail", "exact-view", 20,
			append(subject, exact(), relevance(0.1))...),
		cand("tasks", "alias-view", 19,
			append(subject, func(c *recall.Candidate) {
				c.MatchSignals = append(c.MatchSignals, recall.MatchAlias)
			}, relevance(0.1))...),
		cand("docs", "answer.md#1", 1, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassNaturalLanguage

	got := fuse(t, r, req)
	if got.Results[0].Primary.Locator.String() != "docs:answer.md#1" {
		t.Fatalf("order = %v, want split signals to remain ordinary scored evidence", order(got))
	}
	for _, result := range got.Results {
		if result.Explanation.ExactPromoted {
			t.Fatalf("%s assembled exact+alias across candidates", result.Primary.Locator)
		}
	}
}

func TestIdentifierQueryStillPartitionsExactMatch(t *testing.T) {
	r := newRanker(t, nil)
	req := request(
		cand("mail", "project-health", 20, exact(), relevance(0.1)),
		cand("docs", "answer.md#1", 1, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassIdentifier

	got := fuse(t, r, req)
	if got.Results[0].Primary.Locator.String() != "mail:project-health" {
		t.Fatalf("order = %v, want the named record first", order(got))
	}
	if !got.Results[0].Explanation.ExactPromoted {
		t.Fatal("identifier query did not report exact promotion")
	}
}

func TestStableIdentifierPromotesOnlyTheExactCandidateItNames(t *testing.T) {
	r := newRanker(t, nil)
	req := request(
		cand("mail", "clara", 1, exact(), relevance(0.1)),
		cand("tasks", "td-6c98c1", 2, exact(), relevance(0.1)),
		cand("docs", "answer.md#1", 1, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassIdentifier
	req.StableIdentifiers = []string{"td-6c98c1"}

	got := fuse(t, r, req)
	if want := []string{"tasks:td-6c98c1", "docs:answer.md#1", "mail:clara"}; !reflect.DeepEqual(order(got), want) {
		t.Fatalf("order = %v, want %v", order(got), want)
	}
	if !got.Results[0].Explanation.ExactPromoted {
		t.Fatal("named td candidate did not partition")
	}
	if got.Results[2].Explanation.ExactPromoted {
		t.Fatal("unrelated exact project-name candidate partitioned")
	}
}

func TestStableIdentifierCorrelationCoversInTreeIdentityFields(t *testing.T) {
	r := newRanker(t, nil)
	tests := []struct {
		name       string
		identifier string
		target     recall.Candidate
	}{
		{"tasks id", "aaaa0001", cand("tasks", "aaaa0001", 20, exact(), relevance(0.1))},
		{"ongoing underscore id", "project_recall", cand("tasks", "project_recall", 20, exact(), relevance(0.1))},
		{"ongoing project name", "epub_to_audiobook", cand("tasks", "project-generated-id", 20,
			title("epub_to_audiobook"), exact(), relevance(0.1))},
		{"ongoing relative path", "tools/epub_to_audiobook", cand("tasks", "project-generated-id", 20,
			meta("relative_path", "tools/epub_to_audiobook"), exact(), relevance(0.1))},
		{"document path", "projects/recall/architecture.md", cand("docs", "chunk-1", 20,
			recordID("projects/recall/architecture.md"), exact(), relevance(0.1))},
		{"document basename", "backup-restore.md", cand("docs", "chunk-1", 20,
			recordID("runbooks/backup-restore.md"), exact(), relevance(0.1))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := request(
				cand("mail", "clara", 1, exact(), relevance(0.1)),
				tc.target,
				cand("docs", "answer.md#1", 1, relevance(0.9)),
			)
			req.QueryClass = ranking.QueryClassIdentifier
			req.StableIdentifiers = []string{tc.identifier}
			got := fuse(t, r, req)
			if got.Results[0].Primary.Locator != tc.target.Locator {
				t.Fatalf("order = %v, want %s first", order(got), tc.target.Locator)
			}
			if !got.Results[0].Explanation.ExactPromoted {
				t.Fatal("named candidate did not report promotion")
			}
		})
	}
}

func TestStableIdentifierCorrelationDoesNotScanArbitraryMetadata(t *testing.T) {
	r := newRanker(t, nil)
	req := request(
		cand("mail", "clara", 1, exact(), meta("description", "address td-6c98c1"), relevance(0.1)),
		cand("docs", "answer.md#1", 1, relevance(0.9)),
	)
	req.QueryClass = ranking.QueryClassIdentifier
	req.StableIdentifiers = []string{"td-6c98c1"}

	got := fuse(t, r, req)
	if got.Results[0].Primary.Locator.String() != "docs:answer.md#1" {
		t.Fatalf("order = %v, arbitrary prose metadata correlated as identity", order(got))
	}
	if got.Results[1].Explanation.ExactPromoted {
		t.Fatal("candidate promoted from identifier text in arbitrary metadata")
	}
}

// A source that returns the same record twice contributes its best view of it.
// Scoring the later hit instead would let a source lower its own record by
// finding it again.
func TestGroupUsesBestRankPerSource(t *testing.T) {
	r := newRanker(t, nil)

	best := single(t, fuse(t, r, request(
		cand("docs", "a.md#9", 2, from("tasks:td-1")),
	)))
	both := single(t, fuse(t, r, request(
		cand("docs", "a.md#1", 5, from("tasks:td-1")),
		cand("docs", "a.md#9", 2, from("tasks:td-1")),
	)))

	if both.Score != best.Score {
		t.Errorf("score = %v, want the best rank's %v", both.Score, best.Score)
	}
	if both.Primary.LocalRank != 2 {
		t.Errorf("primary local rank = %d, want the best view of the record", both.Primary.LocalRank)
	}
	if n := both.Explanation.LocalPoolSize; n != 2 {
		t.Errorf("local pool size = %d, want the list the rank came from", n)
	}
}

// Invariant 1. Native scores use incomparable scales, so fusion consumes rank
// only. Handing one source scores in the thousands and another scores below one
// must change nothing at all.
func TestRawLocalScoreIsNeverComparedAcrossSources(t *testing.T) {
	r := newRanker(t, nil)

	// docs ranked its hit first, tasks ranked its hit second. Rank says docs
	// wins; local score, if anyone read it, would say tasks wins by 10000x.
	loud := request(
		cand("tasks", "td-1", 2, score(9000)),
		cand("docs", "spec.md#1", 1, score(0.42)),
	)
	quiet := request(
		cand("tasks", "td-1", 2, score(0.01)),
		cand("docs", "spec.md#1", 1, score(88000)),
	)
	none := request(
		cand("tasks", "td-1", 2),
		cand("docs", "spec.md#1", 1),
	)

	base := fuse(t, r, none)
	if want := []string{"docs:spec.md#1", "tasks:td-1"}; !reflect.DeepEqual(order(base), want) {
		t.Fatalf("order = %v, want %v: rank alone decides", order(base), want)
	}
	for name, req := range map[string]ranking.Request{"loud source wins": loud, "quiet source wins": quiet} {
		t.Run(name, func(t *testing.T) {
			got := fuse(t, r, req)
			if !reflect.DeepEqual(order(got), order(base)) {
				t.Errorf("order = %v, want %v", order(got), order(base))
			}
			for i, res := range got.Results {
				if res.Score != base.Results[i].Score {
					t.Errorf("result %d scored %v, want %v", i, res.Score, base.Results[i].Score)
				}
			}
		})
	}
}

// A summary of two tasks is worth showing and is not a third opinion. Its own
// score may be the highest in the cluster and it still must not displace the
// records it restates.
func TestCompositeDoesNotCorroborateItsSources(t *testing.T) {
	r := newRanker(t, nil)
	project := []opt{meta(ranking.MetaEntityID, "proj-recall")}

	withoutSummary := single(t, fuse(t, r, request(
		cand("tasks", "td-1", 1, project...),
		cand("tasks", "td-2", 2, project...),
	)))

	withSummary := single(t, fuse(t, r, request(
		cand("tasks", "td-1", 1, project...),
		cand("tasks", "td-2", 2, project...),
		cand("docs", "weekly.md#1", 1, append(project, from("tasks:td-1", "tasks:td-2"))...),
	)))

	if withSummary.Score != withoutSummary.Score {
		t.Errorf("score = %v, want %v: a restatement is not new evidence",
			withSummary.Score, withoutSummary.Score)
	}
	if n := withSummary.Explanation.Corroboration.IndependentUnits; n != 2 {
		t.Errorf("distinct lineages = %d, want 2", n)
	}
	// It is still shown: display and corroboration are different questions.
	if len(withSummary.Members) != 3 {
		t.Errorf("members = %d, want the summary displayed alongside its sources",
			len(withSummary.Members))
	}
}

// A source that declares it mirrors another cannot say which record it mirrors,
// so it never moves a root — but it must not be counted as a second opinion.
func TestMirrorSourceDoesNotCorroborate(t *testing.T) {
	r := newRanker(t, nil)
	person := []opt{kind(recall.RecordPerson), meta(ranking.MetaEntityID, "person-42")}

	req := request(
		cand("tasks", "td-1", 1, person...),
		cand("signals", "mirror-1", 1, person...),
	)
	req.SourceDerivations = map[recall.SourceUID]recall.SourceUID{"uid-signals": "uid-tasks"}

	got := single(t, fuse(t, r, req))
	if n := got.Explanation.Corroboration.IndependentUnits; n != 1 {
		t.Errorf("distinct lineages = %d, want 1: a whole-source projection agrees with itself", n)
	}
	alone := single(t, fuse(t, r, request(cand("tasks", "td-1", 1, person...))))
	if got.Score != alone.Score {
		t.Errorf("score = %v, want %v", got.Score, alone.Score)
	}
}

// Two chunks of one document are one record. Fusion learns that from the
// source record identifier, not from the locators, which differ by design.
func TestChunksOfOneRecordDoNotCorroborate(t *testing.T) {
	r := newRanker(t, nil)
	got := single(t, fuse(t, r, request(
		cand("docs", "spec.md#ranking", 1, recordID("spec.md")),
		cand("docs", "spec.md#lineage", 2, recordID("spec.md")),
	)))

	if n := got.Explanation.Corroboration.IndependentUnits; n != 1 {
		t.Errorf("distinct lineages = %d, want 1", n)
	}
	alone := single(t, fuse(t, r, request(cand("docs", "spec.md#ranking", 1, recordID("spec.md")))))
	if got.Score != alone.Score {
		t.Errorf("score = %v, want %v: a second chunk is not a second opinion", got.Score, alone.Score)
	}
	if len(got.Members) != 2 {
		t.Errorf("members = %d, want both chunks addressable", len(got.Members))
	}
}

// A body-less heading is an excellent label and a poor answer. It may earn the
// document's score — every query term can be concentrated in four words — but
// a matched content chunk of that same record is the useful pointer to show.
// Representation and scoring are separate decisions: the heading remains the
// score basis, so the explanation never attributes its arithmetic to the
// lower-scoring content chunk.
func TestMatchedChunkRepresentsDocumentWithoutChangingScoreBasis(t *testing.T) {
	r := newRanker(t, nil)
	heading := cand("docs", "research.md#L1-L1", 1,
		recordID("research.md"),
		excerptKind(recall.ExcerptPreview),
		relevance(0.95),
	)
	content := cand("docs", "research.md#L20-L27", 2,
		recordID("research.md"),
		excerptKind(recall.ExcerptMatched),
		relevance(0.40),
	)

	got := single(t, fuse(t, r, request(heading, content)))
	headingOnly := single(t, fuse(t, r, request(heading)))

	if got.Primary.Locator != content.Locator {
		t.Fatalf("primary = %s, want matched content chunk %s", got.Primary.Locator, content.Locator)
	}
	if got.Score != headingOnly.Score {
		t.Errorf("score = %v, want heading-derived %v: representation must not rerank the record",
			got.Score, headingOnly.Score)
	}
	if got.Explanation.LineageRoot != "uid-docs:research.md#L1-L1" ||
		got.Explanation.LocalRank != heading.LocalRank ||
		got.Explanation.Relevance == nil ||
		*got.Explanation.Relevance != *heading.Relevance {
		t.Errorf("score basis = %+v, want the heading's lineage, rank, and relevance",
			got.Explanation)
	}
	if got.Explanation.Score != got.Score {
		t.Errorf("explanation claims %v but result scored %v", got.Explanation.Score, got.Score)
	}
	if len(got.Members) != 2 {
		t.Errorf("members = %d, want both chunks retrievable", len(got.Members))
	}
}

func TestDocumentRepresentativePreferenceIsNarrow(t *testing.T) {
	r := newRanker(t, nil)
	tests := []struct {
		name string
		kind recall.RecordType
		from recall.ExcerptKind
		want string
	}{
		{
			name: "structured record keeps score winner",
			kind: recall.RecordTask,
			from: recall.ExcerptPreview,
			want: "record#label",
		},
		{
			name: "empty excerpt kind is neutral",
			kind: recall.RecordDocument,
			from: "",
			want: "record#label",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := single(t, fuse(t, r, request(
				cand("docs", "record#label", 1,
					kind(tc.kind), recordID("record"), excerptKind(tc.from), relevance(0.95)),
				cand("docs", "record#content", 2,
					kind(tc.kind), recordID("record"), excerptKind(recall.ExcerptMatched), relevance(0.40)),
			)))
			if got.Primary.Locator.Local != tc.want {
				t.Errorf("primary = %s, want score winner %s", got.Primary.Locator.Local, tc.want)
			}
		})
	}
}

func TestDocumentRepresentativeDoesNotBreakScoreTie(t *testing.T) {
	r := newRanker(t, nil)
	pool := []recall.Candidate{
		cand("docs", "z.md#L1-L1", 1,
			recordID("z.md"), excerptKind(recall.ExcerptPreview), relevance(0.5)),
		cand("docs", "z.md#L20-L25", 2,
			recordID("z.md"), excerptKind(recall.ExcerptMatched), relevance(0.2)),
		cand("docs", "z.md#L10-L15", 1,
			recordID("other.md"), excerptKind(recall.ExcerptMatched), relevance(0.5)),
	}

	want := []string{"docs:z.md#L20-L25", "docs:z.md#L10-L15"}
	reversed := slices.Clone(pool)
	slices.Reverse(reversed)
	for _, in := range [][]recall.Candidate{pool, reversed} {
		got := fuse(t, r, request(in...))
		if order := order(got); !reflect.DeepEqual(order, want) {
			t.Errorf("order = %v, want %v: display representative became a tie-breaker", order, want)
		}
	}
}

// Every configured value that moved this result has to be readable off the
// explanation. A setting that cannot appear in one does not exist.
func TestExplanationCarriesEveryConfiguredValue(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.RankConstant = 40
		c.CorroborationCap = 1.5
		c.Sources["uid-tasks"] = ranking.SourceConfig{
			SourceID:  "tasks",
			BasePrior: 1.0,
			IntentPriors: []ranking.IntentPrior{
				{QueryClass: "task", Effective: 1.5},
				{QueryClass: "person", Effective: 0.6},
			},
		}
	})

	req := request(
		cand("tasks", "td-1", 3, exact(), meta(ranking.MetaEntityID, "proj-recall")),
		cand("docs", "spec.md#1", 1, meta(ranking.MetaEntityID, "proj-recall")),
	)
	req.QueryClass = "task"

	got := single(t, fuse(t, r, req))
	e := got.Explanation

	if e.SourceID != "tasks" || e.SourceUID != "uid-tasks" {
		t.Errorf("source = %s/%s, want tasks/uid-tasks", e.SourceID, e.SourceUID)
	}
	if e.LocalRank != 3 || e.LocalPoolSize != 1 {
		t.Errorf("local rank %d of pool %d, want 3 of 1", e.LocalRank, e.LocalPoolSize)
	}
	if e.RankConstant != 40 {
		t.Errorf("rank constant = %v, want the configured 40", e.RankConstant)
	}
	if e.Prior.Base != 1.0 {
		t.Errorf("base prior = %v, want 1.0", e.Prior.Base)
	}
	if e.Prior.Intent != 0.5 || e.Prior.Rule != "task" {
		t.Errorf("intent = %v by rule %q, want 0.5 attributed to the task class",
			e.Prior.Intent, e.Prior.Rule)
	}
	if e.Prior.Effective != 1.5 {
		t.Errorf("effective prior = %v, want the class prior 1.5", e.Prior.Effective)
	}
	if e.LineageRoot != "uid-tasks:td-1" {
		t.Errorf("lineage root = %q", e.LineageRoot)
	}
	if e.Corroboration.Cap != 1.5 || e.Corroboration.IndependentUnits != 2 {
		t.Errorf("corroboration = %+v, want cap 1.5 over 2 lineages", e.Corroboration)
	}
	if !e.ExactPromoted {
		t.Error("exact promotion not explained")
	}
	// The arithmetic the explanation claims must be the arithmetic that ran.
	want := 1.5/(40+3) + 1.0/(40+1)
	if e.Corroboration.CapApplied {
		want = 1.5 * 1.5 / (40 + 3)
	}
	// A tolerance, not equality: this is a sum, and the order the terms are
	// associated in is an implementation detail worth one ULP. Equality is
	// still the right assertion where the arithmetic must be identical rather
	// than equivalent, as in the duplicate-lineage test.
	if math.Abs(got.Score-want) > 1e-12 {
		t.Errorf("score = %v, want %v", got.Score, want)
	}
	if e.Score != got.Score {
		t.Errorf("explanation claims %v but the result scored %v", e.Score, got.Score)
	}
}

// An intent rule that does not fire must leave no trace: an explanation
// reporting an adjustment nobody applied is worse than none.
func TestIntentPriorOnlyAppliesToItsQueryClass(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.Sources["uid-tasks"] = ranking.SourceConfig{
			SourceID:     "tasks",
			BasePrior:    1,
			IntentPriors: []ranking.IntentPrior{{QueryClass: "person", Effective: 1.8}},
		}
	})

	req := request(cand("tasks", "td-1", 1))
	req.QueryClass = "task"

	e := single(t, fuse(t, r, req)).Explanation
	if e.Prior.Intent != 0 || e.Prior.Rule != "" {
		t.Errorf("prior = %+v, want no rule reported when none fired", e.Prior)
	}
	if e.Prior.Effective != 1 {
		t.Errorf("effective prior = %v, want the base 1", e.Prior.Effective)
	}
}

// Suppression filters passive display. An explicit request is someone asking,
// and the answer never hides what they asked for.
func TestLineageSuppressionAppliesToPreReplyOnly(t *testing.T) {
	r := newRanker(t, nil)
	pool := []recall.Candidate{
		cand("tasks", "td-1", 1),
		cand("docs", "spec.md#1", 1),
	}

	for _, tc := range []struct {
		mode      recall.InvocationMode
		wantShown []string
		wantSup   int
	}{
		{recall.ModePreReply, []string{"docs:spec.md#1"}, 1},
		{recall.ModeExplicit, []string{"docs:spec.md#1", "tasks:td-1"}, 0},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			req := request(pool...)
			req.Mode = tc.mode
			req.SuppressLineages = []recall.LineageRoot{"uid-tasks:td-1"}

			got := fuse(t, r, req)
			if !reflect.DeepEqual(order(got), tc.wantShown) {
				t.Errorf("results = %v, want %v", order(got), tc.wantShown)
			}
			if len(got.Suppressed) != tc.wantSup {
				t.Errorf("suppressed = %+v, want %d records", got.Suppressed, tc.wantSup)
			}
			if tc.wantSup > 0 {
				s := got.Suppressed[0]
				if s.Reason != recall.SuppressLineageSeen || s.LineageRoot != "uid-tasks:td-1" || s.Count != 1 {
					t.Errorf("suppression = %+v, want the seen lineage counted", s)
				}
			}
		})
	}
}

// Showing the same paragraph twice is a bad answer, so selection withholds the
// repeat and says why — but only within one source.
//
// Across sources it must not: suppression keyed on an advisory hash would let
// any source remove another's evidence from a response by echoing its
// fingerprint and scoring high enough to be considered first. Seeing the same
// text from two sources is a cosmetic cost; silently dropping a source's
// evidence on another source's say-so is not.
func TestNearDuplicateIsSuppressedWithinASourceOnly(t *testing.T) {
	r := newRanker(t, nil)

	withinOne := fuse(t, r, request(
		cand("docs", "spec.md#1", 1, fingerprint("fp-same")),
		cand("docs", "copy.md#1", 2, kind(recall.RecordTask), fingerprint("fp-same")),
	))
	if len(withinOne.Results) != 1 {
		t.Fatalf("results = %v, want the source's own repeat withheld", order(withinOne))
	}
	if len(withinOne.Suppressed) != 1 || withinOne.Suppressed[0].Reason != recall.SuppressDuplicate {
		t.Fatalf("suppressed = %+v, want one near_duplicate", withinOne.Suppressed)
	}

	acrossSources := fuse(t, r, request(
		cand("docs", "spec.md#1", 1, fingerprint("fp-same")),
		cand("tasks", "td-1", 1, kind(recall.RecordTask), fingerprint("fp-same")),
	))
	if len(acrossSources.Results) != 2 {
		t.Fatalf("results = %v, want both kept: one source may not hide another",
			order(acrossSources))
	}
	if len(acrossSources.Suppressed) != 0 {
		t.Fatalf("suppressed = %+v, want nothing withheld across sources",
			acrossSources.Suppressed)
	}
}

// Diversity is a selection policy applied after relevance, never a substitute
// for it. A source's surplus results are demoted, not deleted, and they are
// only reported as suppressed when the policy actually cost them a place.
func TestDiversityDemotesRatherThanReordersRelevance(t *testing.T) {
	pool := []recall.Candidate{
		cand("tasks", "td-1", 1),
		cand("tasks", "td-2", 2),
		cand("tasks", "td-3", 3),
		cand("docs", "spec.md#1", 4),
	}

	t.Run("room for everything", func(t *testing.T) {
		r := newRanker(t, func(c *ranking.Config) { c.MaxPerSource = 2 })
		got := fuse(t, r, request(pool...))
		want := []string{"tasks:td-1", "tasks:td-2", "docs:spec.md#1", "tasks:td-3"}
		if !reflect.DeepEqual(order(got), want) {
			t.Errorf("order = %v, want %v", order(got), want)
		}
		if len(got.Suppressed) != 0 {
			t.Errorf("suppressed = %+v, want none: nothing lost a place", got.Suppressed)
		}
	})

	t.Run("policy costs a place", func(t *testing.T) {
		r := newRanker(t, func(c *ranking.Config) { c.MaxPerSource = 2; c.Limit = 3 })
		got := fuse(t, r, request(pool...))
		want := []string{"tasks:td-1", "tasks:td-2", "docs:spec.md#1"}
		if !reflect.DeepEqual(order(got), want) {
			t.Errorf("order = %v, want %v", order(got), want)
		}
		if len(got.Suppressed) != 1 || got.Suppressed[0].Reason != recall.SuppressDiversity {
			t.Errorf("suppressed = %+v, want td-3 reported as a diversity suppression", got.Suppressed)
		}
		if got.Truncated {
			t.Error("truncated set for a result the diversity policy withheld")
		}
	})

	t.Run("limit truncates", func(t *testing.T) {
		r := newRanker(t, func(c *ranking.Config) { c.Limit = 2 })
		got := fuse(t, r, request(pool...))
		if !got.Truncated || got.Dropped != 2 {
			t.Errorf("truncated = %v after dropping %d, want 2 dropped", got.Truncated, got.Dropped)
		}
	})
}

// A defect in the pool is never a quietly worse ranking. A missing rank would
// score as better than rank 1; a source with no configured prior means the
// caller searched something the plan never resolved.
func TestFusionRefusesUnrankableInput(t *testing.T) {
	r := newRanker(t, nil)

	t.Run("no local rank", func(t *testing.T) {
		_, err := r.Fuse(request(cand("tasks", "td-1", 0)))
		if !errors.Is(err, ranking.ErrCandidate) {
			t.Fatalf("err = %v, want ErrCandidate", err)
		}
	})

	t.Run("source with no prior", func(t *testing.T) {
		bare, err := ranking.New(ranking.Config{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = bare.Fuse(request(cand("tasks", "td-1", 1)))
		if !errors.Is(err, ranking.ErrConfig) {
			t.Fatalf("err = %v, want ErrConfig", err)
		}
	})

	t.Run("no resolver", func(t *testing.T) {
		_, err := r.Fuse(ranking.Request{Candidates: []recall.Candidate{cand("tasks", "td-1", 1)}})
		if !errors.Is(err, ranking.ErrRequest) {
			t.Fatalf("err = %v, want ErrRequest", err)
		}
	})

	// An empty pool is a legitimate answer, not a failure: the sources answered
	// and had nothing.
	t.Run("empty pool", func(t *testing.T) {
		got, err := r.Fuse(ranking.Request{})
		if err != nil || len(got.Results) != 0 {
			t.Fatalf("got %+v, %v; want an empty fusion", got, err)
		}
	})
}

func relevance(v float64) opt { return func(c *recall.Candidate) { c.Relevance = &v } }

// TestPriorAloneNoLongerDecidesBetweenTopHits is td-aefb1d, in the shape the
// eval pack's dentist-001 case records it.
//
// Two sources each return their own rank 1. Before relevance, prior was the
// only thing separating them, so the more-trusted source won regardless of
// whether its hit had anything to do with the query — and on the home profile a
// 7% prior edge bought five rank positions, which is how five projects with no
// textual relationship to a question outranked the document that answered it.
func TestPriorAloneNoLongerDecidesBetweenTopHits(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.Sources["uid-docs"] = ranking.SourceConfig{SourceID: "docs", BasePrior: 1.5}
		c.Sources["uid-tasks"] = ranking.SourceConfig{SourceID: "tasks", BasePrior: 1.4}
	})

	// Both are their source's first hit. The document mentions the word once in
	// four hundred; the task IS the word.
	got := fuse(t, r, request(
		cand("docs", "gog/README.md#L30", 1, relevance(0.10)),
		cand("tasks", "td-1", 1, relevance(0.93)),
	))
	if want := []string{"tasks:td-1", "docs:gog/README.md#L30"}; !reflect.DeepEqual(order(got), want) {
		t.Errorf("order = %v, want %v: the higher prior must not win on prior alone "+
			"when the other source is far more about the query", order(got), want)
	}

	// The prior still means something: equal relevance returns the decision to it.
	tied := fuse(t, r, request(
		cand("docs", "gog/README.md#L30", 1, relevance(0.5)),
		cand("tasks", "td-1", 1, relevance(0.5)),
	))
	if want := []string{"docs:gog/README.md#L30", "tasks:td-1"}; !reflect.DeepEqual(order(tied), want) {
		t.Errorf("order = %v, want %v: relevance scales the prior, it does not replace it",
			order(tied), want)
	}
}

// A source that reports no relevance must rank exactly where it did before the
// field existed. This is what keeps an out-of-tree adapter — the gog calendar
// and mail adapters live outside this repository — working across the change.
func TestMissingRelevanceReproducesThePreviousOrdering(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.Sources["uid-docs"] = ranking.SourceConfig{SourceID: "docs", BasePrior: 1.5}
		c.Sources["uid-tasks"] = ranking.SourceConfig{SourceID: "tasks", BasePrior: 1.4}
	})

	silent := fuse(t, r, request(
		cand("docs", "spec.md#1", 1),
		cand("tasks", "td-1", 1),
	))
	perfect := fuse(t, r, request(
		cand("docs", "spec.md#1", 1, relevance(1)),
		cand("tasks", "td-1", 1, relevance(1)),
	))
	if !reflect.DeepEqual(order(silent), order(perfect)) {
		t.Fatalf("silent order %v, explicit-1.0 order %v", order(silent), order(perfect))
	}
	for i, res := range silent.Results {
		if res.Score != perfect.Results[i].Score {
			t.Errorf("result %d scored %v silent, %v at relevance 1.0: absent must mean 1.0 exactly",
				i, res.Score, perfect.Results[i].Score)
		}
	}
}

// Out-of-range values cost the source its position rather than failing the
// query: one adapter's arithmetic bug must not take the whole answer down.
func TestRelevanceOutOfRangeIsClampedNotFatal(t *testing.T) {
	r := newRanker(t, nil)
	for name, v := range map[string]float64{
		"negative":  -3,
		"above one": 40,
		"nan":       math.NaN(),
	} {
		t.Run(name, func(t *testing.T) {
			got := fuse(t, r, request(
				cand("docs", "spec.md#1", 1, relevance(v)),
				cand("tasks", "td-1", 1, relevance(0.5)),
			))
			if len(got.Results) != 2 {
				t.Fatalf("results = %d, want both candidates kept", len(got.Results))
			}
			for _, res := range got.Results {
				if math.IsNaN(res.Score) || res.Score < 0 {
					t.Errorf("score = %v, want a usable number", res.Score)
				}
			}
		})
	}
}

// Relevance travels with the rank it belongs to. A record seen twice by one
// source must be scored with the relevance of the candidate that earned the
// best rank, not the best relevance found anywhere in the group.
func TestRelevancePairsWithTheRankItBelongsTo(t *testing.T) {
	r := newRanker(t, nil)

	got := single(t, fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.2), from("tasks:td-1")),
		cand("docs", "a.md#9", 4, relevance(0.9), from("tasks:td-1")),
	)))
	want := single(t, fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.2), from("tasks:td-1")),
	)))
	if got.Score != want.Score {
		t.Errorf("score = %v, want %v: the rank-1 candidate's own relevance, not the "+
			"0.9 belonging to the rank-4 one", got.Score, want.Score)
	}
}

// The volume rules of td-57b319. Thirteen sources at a per-source cap of twenty
// meant a natural-language query's length was the profile's arithmetic and not
// a fact about the corpus: admission decided WHICH candidates filled each cap,
// and nothing decided how many came back. These are the two rules that replaced
// it, and the four things neither of them is allowed to do.

// The floor is expressed in relevance because relevance is the only
// cross-source-comparable signal fusion is permitted to read.
func TestRelevanceFloorWithholdsResultsAndCountsThem(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.05 })

	got := fuse(t, r, request(
		cand("docs", "about.md#1", 1, relevance(0.6)),
		cand("docs", "mention.md#1", 2, relevance(0.04)),
		cand("tasks", "td-1", 1, relevance(0)),
	))

	if want := []string{"docs:about.md#1"}; !reflect.DeepEqual(order(got), want) {
		t.Errorf("results = %v, want %v", order(got), want)
	}
	// Named as well as counted, like every other withheld cluster: an answer
	// that silently dropped what a source returned reads as a corpus that never
	// held it.
	want := []recall.Suppression{
		{Reason: recall.SuppressRelevanceFloor, Count: 1, LineageRoot: "uid-docs:mention.md#1"},
		{Reason: recall.SuppressRelevanceFloor, Count: 1, LineageRoot: "uid-tasks:td-1"},
	}
	if !reflect.DeepEqual(got.Suppressed, want) {
		t.Errorf("suppressed = %+v, want %+v", got.Suppressed, want)
	}
}

// A cluster survives if ANY record in it clears the floor. The unit is what the
// caller is shown, and a cluster is one thing shown: judging it by a single
// member would withhold a record on the strength of the weakest view of it.
func TestRelevanceFloorKeepsAClusterAnyRecordOfWhichIsRelevant(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.5 })

	got := fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.9), recordID("a.md")),
		cand("docs", "a.md#9", 2, relevance(0.01), recordID("a.md")),
	))

	if len(got.Results) != 1 || len(got.Suppressed) != 0 {
		t.Errorf("results = %v, suppressed = %+v; want the one record kept whole",
			order(got), got.Suppressed)
	}
}

// A record the host says it has already shown stays suppressed even when the
// view carrying that lineage root is the one below the floor.
//
// This is why the floor withholds clusters and not candidates. Dropping the
// weak view before grouping took it out of the cluster's view classes, and
// allSuppressed — which requires EVERY class to hold a suppressed root — then
// re-displayed the record under the very view the host had suppressed, which
// is the failure its own doc comment describes.
func TestRelevanceFloorDoesNotDefeatHostLineageSuppression(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.05 })

	view := func(sourceID string, rel float64) recall.Candidate {
		return cand(sourceID, "rec-1", 1, relevance(rel),
			recordID("rec-1"), fingerprint("fp-1"), revision("rev-1"))
	}
	req := request(view("docs", 0.9), view("mail", 0.01))
	req.Mode = recall.ModePreReply
	req.SuppressLineages = []recall.LineageRoot{"uid-mail:rec-1"}

	if got := fuse(t, r, req); len(got.Results) != 0 {
		t.Errorf("results = %v, want none: the host has already shown this record", order(got))
	}
}

// Which chunk of a document is DISPLAYED must not change because of the floor.
//
// Relevance is coverage times concentration, so a short heading scores higher
// than the long body chunk that answers the question. Withholding the body
// chunk as a candidate left the heading representing the document and the
// caller reading its title back — td-7b28b9's defect, arriving through a rule
// that has nothing to do with representation.
func TestRelevanceFloorDoesNotChangeTheDisplayedChunk(t *testing.T) {
	displayed := func(floor float64) string {
		t.Helper()
		r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = floor })
		got := single(t, fuse(t, r, request(
			cand("docs", "doc.md#L1-L1", 1, relevance(0.9),
				recordID("doc.md"), excerptKind(recall.ExcerptPreview)),
			cand("docs", "doc.md#L5-L8", 2, relevance(0.02),
				recordID("doc.md"), excerptKind(recall.ExcerptMatched)),
		)))
		return got.Primary.Locator.String()
	}

	if got, want := displayed(0.05), displayed(0); got != want {
		t.Errorf("displayed %q with the floor and %q without it", got, want)
	}
}

// A malformed relevance costs a source its position, never its presence.
//
// relevanceOf clamps a number that is not a number to 0, which is the right
// price when the value only orders things. Under a floor that price becomes
// disappearance from every answer, which an adapter computing matched/total
// with total = 0 would earn silently.
func TestMalformedRelevanceIsNotAFloorVerdict(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.5 })

	nan := math.NaN()
	inf := math.Inf(1)
	got := fuse(t, r, request(
		cand("docs", "nan.md#1", 1, func(c *recall.Candidate) { c.Relevance = &nan }),
		cand("docs", "inf.md#1", 2, func(c *recall.Candidate) { c.Relevance = &inf }),
		cand("docs", "honest.md#1", 3, relevance(0.1)),
	))

	want := []string{"docs:inf.md#1", "docs:nan.md#1"}
	if got := slices.Sorted(slices.Values(order(got))); !slices.Equal(got, want) {
		t.Errorf("results = %v, want the two unusable numbers kept and the honest 0.1 withheld", got)
	}
}

// The two exemptions, both structural. A record named outright need not
// describe itself, and a source that reports no relevance may not be punished
// by a rule written in the number it did not report.
func TestRelevanceFloorExemptsExactMatchesAndSilentSources(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.5 })

	got := fuse(t, r, request(
		cand("tasks", "aaaa0001", 1, exact(), relevance(0)),
		cand("docs", "silent.md#1", 1),
		cand("docs", "weak.md#1", 2, relevance(0.4)),
	))

	want := []string{"docs:silent.md#1", "tasks:aaaa0001"}
	if got := slices.Sorted(slices.Values(order(got))); !slices.Equal(got, want) {
		t.Errorf("results = %v, want the exact hit and the silent source kept", got)
	}
}

// A floor may withhold; it may not abstain. An empty answer is read everywhere
// as a claim about the corpus — the CLI exits 2, the MCP text tells a model
// that reporting nothing was found is supported, and the live profile's
// must_abstain check rests on nothing being able to turn "something" back into
// "nothing". A threshold somebody picked may not make that claim, so when
// nothing clears the floor the floor withholds nothing.
func TestRelevanceFloorNeverEmptiesTheAnswer(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.5 })

	got := fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.1)),
		cand("docs", "b.md#1", 2, relevance(0.01)),
	))

	if len(got.Results) != 2 || len(got.Suppressed) != 0 {
		t.Errorf("results = %v, suppressed = %+v; want the weakest evidence there is",
			order(got), got.Suppressed)
	}

	// One survivor is enough for the floor to apply again: the exception is
	// about not manufacturing an absence, not about a grace slot.
	got = fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.9)),
		cand("docs", "b.md#1", 2, relevance(0.01)),
	))
	if want := []string{"docs:a.md#1"}; !reflect.DeepEqual(order(got), want) {
		t.Errorf("results = %v, want %v", order(got), want)
	}
}

// The rules compose, so "never empties the answer" has to be tested against
// what SURVIVED the others and not against the whole cluster list.
//
// Found by a review probe: a floor that kept its one weak result while lineage
// suppression removed the strong one still produced the empty answer, one rule
// each, and neither rule alone had emptied anything.
func TestRelevanceFloorNeverEmptiesTheAnswerAlongsideOtherRules(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.10 })

	req := request(
		cand("docs", "seen.md#1", 1, relevance(0.9)),
		cand("tasks", "weak-1", 1, relevance(0.02)),
	)
	req.Mode = recall.ModePreReply
	req.SuppressLineages = []recall.LineageRoot{"uid-docs:seen.md#1"}

	got := fuse(t, r, req)
	if want := []string{"tasks:weak-1"}; !reflect.DeepEqual(order(got), want) {
		t.Errorf("results = %v, want %v: the host has seen the strong record, and a "+
			"floor may not turn what is left into an abstention", order(got), want)
	}
	for _, s := range got.Suppressed {
		if s.Reason == recall.SuppressRelevanceFloor {
			t.Errorf("suppressed = %+v, want no floor withholding: it stood down", got.Suppressed)
		}
	}
}

// The local pool size is the list the RANK came from — the source's own answer.
// A core-side floor narrows what is shown, not what a source said, so an
// explanation reading "rank 1 of 3" must keep saying so after two of the three
// are withheld.
func TestRelevanceFloorDoesNotRewriteTheLocalPoolSize(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = 0.5 })

	got := single(t, fuse(t, r, request(
		cand("docs", "a.md#1", 1, relevance(0.9)),
		cand("docs", "b.md#1", 2, relevance(0.1)),
		cand("docs", "c.md#1", 3, relevance(0.1)),
	)))

	if got.Explanation.LocalPoolSize != 3 {
		t.Errorf("local pool size = %d, want 3: the floor did not shorten what docs answered",
			got.Explanation.LocalPoolSize)
	}
}

// Lineage is a fact about the corpus, not about what this answer chose to show.
// A derivation chain whose middle link the floor dropped must still reach the
// original record, or one display rule would silently change how a record
// deduplicates.
func TestRelevanceFloorDoesNotShortenADerivationChain(t *testing.T) {
	chain := func(floor float64) recall.LineageRoot {
		t.Helper()
		r := newRanker(t, func(c *ranking.Config) { c.RelevanceFloor = floor })
		got := fuse(t, r, request(
			cand("signals", "sig-1", 1, relevance(0.9), from("docs:note.md#1")),
			cand("docs", "note.md#1", 1, relevance(0.01), from("tasks:td-1")),
		))
		if len(got.Results) != 1 {
			t.Fatalf("results = %v, want the one admitted candidate", order(got))
		}
		return got.Results[0].Explanation.LineageRoot
	}

	if got, want := chain(0.5), chain(0); got != want {
		t.Errorf("lineage root = %q with the floor and %q without it", got, want)
	}
}

// The result budget is what stops the answer's length from being the profile's
// arithmetic. It is filled in fused order across every source at once, and it
// reports itself: a caller who cannot tell a short answer from a truncated one
// cannot tell a corpus with two answers from a budget with room for two.
func TestResultBudgetBoundsTheAnswerAndSaysSo(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.Limit = 2 })

	got := fuse(t, r, request(
		cand("docs", "a.md#1", 1),
		cand("docs", "b.md#1", 2),
		cand("docs", "c.md#1", 3),
		cand("tasks", "td-1", 1),
	))

	if len(got.Results) != 2 || !got.Truncated || got.Dropped != 2 || got.Limit != 2 {
		t.Errorf("results = %v, truncated = %v, dropped = %d, limit = %d; want two results, "+
			"two dropped, and the budget reported",
			order(got), got.Truncated, got.Dropped, got.Limit)
	}
}

// A request's limit is what this caller asked for and overrides the profile's;
// the reported budget is the one that applied to them, not the configured one.
func TestReportedBudgetIsTheOneThatApplied(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) { c.Limit = 2 })

	req := request(cand("docs", "a.md#1", 1), cand("docs", "b.md#1", 2), cand("docs", "c.md#1", 3))
	req.Limit = 3
	got := fuse(t, r, req)

	if len(got.Results) != 3 || got.Truncated || got.Limit != 3 {
		t.Errorf("results = %v, truncated = %v, limit = %d; want the request's three",
			order(got), got.Truncated, got.Limit)
	}
}

// A floor outside [0,1) is refused rather than clamped, for the reason every
// other ranking value is: a machine must not rank differently from the
// configuration that was reviewed. One is refused too — on the shared
// definition relevance is exactly 1 for a browse with no query terms and for a
// record whose source could not report a length, so that floor keeps precisely
// the candidates that told fusion nothing.
func TestRelevanceFloorOutsideItsRangeIsRefused(t *testing.T) {
	for _, floor := range []float64{-0.1, 1, 1.5, math.NaN()} {
		cfg := ranking.Config{
			Sources:        map[recall.SourceUID]ranking.SourceConfig{"uid-docs": {SourceID: "docs", BasePrior: 1}},
			RelevanceFloor: floor,
		}
		if _, err := ranking.New(cfg); !errors.Is(err, ranking.ErrConfig) {
			t.Errorf("relevance_floor %v: err = %v, want ErrConfig", floor, err)
		}
	}
}
