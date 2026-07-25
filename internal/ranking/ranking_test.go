package ranking_test

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/recall"
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
			if n := got.Explanation.Corroboration.DistinctLineages; n != 1 {
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
func TestDistinctLineagesCorroborateUpToTheCap(t *testing.T) {
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
	if corr.DistinctLineages != 3 {
		t.Errorf("distinct lineages = %d, want 3", corr.DistinctLineages)
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
	if n := withSummary.Explanation.Corroboration.DistinctLineages; n != 2 {
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
	if n := got.Explanation.Corroboration.DistinctLineages; n != 1 {
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

	if n := got.Explanation.Corroboration.DistinctLineages; n != 1 {
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

// Every configured value that moved this result has to be readable off the
// explanation. A setting that cannot appear in one does not exist.
func TestExplanationCarriesEveryConfiguredValue(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.RankConstant = 40
		c.CorroborationCap = 1.5
		c.Sources["uid-tasks"] = ranking.SourceConfig{
			SourceID:  "tasks",
			BasePrior: 1.2,
			IntentPriors: []ranking.IntentPrior{
				{Rule: "work_items_for_task_queries", QueryClass: "task", Adjustment: 0.5},
				{Rule: "work_items_for_people", QueryClass: "person", Adjustment: -0.4},
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
	if e.Prior.Base != 1.2 {
		t.Errorf("base prior = %v, want 1.2", e.Prior.Base)
	}
	if e.Prior.Intent != 0.5 || e.Prior.Rule != "work_items_for_task_queries" {
		t.Errorf("intent = %v by rule %q, want 0.5 by work_items_for_task_queries",
			e.Prior.Intent, e.Prior.Rule)
	}
	if e.Prior.Effective != 1.7 {
		t.Errorf("effective prior = %v, want 1.7", e.Prior.Effective)
	}
	if e.LineageRoot != "uid-tasks:td-1" {
		t.Errorf("lineage root = %q", e.LineageRoot)
	}
	if e.Corroboration.Cap != 1.5 || e.Corroboration.DistinctLineages != 2 {
		t.Errorf("corroboration = %+v, want cap 1.5 over 2 lineages", e.Corroboration)
	}
	if !e.ExactPromoted {
		t.Error("exact promotion not explained")
	}
	// The arithmetic the explanation claims must be the arithmetic that ran.
	want := 1.7/(40+3) + 1.0/(40+1)
	if e.Corroboration.CapApplied {
		want = 1.5 * 1.7 / (40 + 3)
	}
	if got.Score != want || e.Score != got.Score {
		t.Errorf("score = %v (explained as %v), want %v", got.Score, e.Score, want)
	}
}

// An intent rule that does not fire must leave no trace: an explanation
// reporting an adjustment nobody applied is worse than none.
func TestIntentPriorOnlyAppliesToItsQueryClass(t *testing.T) {
	r := newRanker(t, func(c *ranking.Config) {
		c.Sources["uid-tasks"] = ranking.SourceConfig{
			SourceID:     "tasks",
			BasePrior:    1,
			IntentPriors: []ranking.IntentPrior{{Rule: "people_queries", QueryClass: "person", Adjustment: 0.8}},
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

// Clustering keeps a task and a document apart even when they carry the same
// text, because they are different records. Showing the same paragraph twice is
// still a bad answer, so selection withholds the second one and says why.
func TestNearDuplicateIsSuppressedWithAReason(t *testing.T) {
	r := newRanker(t, nil)
	got := fuse(t, r, request(
		cand("docs", "spec.md#1", 1, fingerprint("fp-same")),
		cand("tasks", "td-1", 1, kind(recall.RecordTask), fingerprint("fp-same")),
	))

	if len(got.Results) != 1 {
		t.Fatalf("results = %v, want one shown", order(got))
	}
	if len(got.Suppressed) != 1 || got.Suppressed[0].Reason != recall.SuppressDuplicate {
		t.Fatalf("suppressed = %+v, want one near_duplicate", got.Suppressed)
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
