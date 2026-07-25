package lineage_test

import (
	"errors"
	"testing"

	"github.com/marcus/recall/internal/lineage"
	"github.com/marcus/recall/internal/recall"
)

var resolver = lineage.MapResolver{
	"tasks":         "uid-tasks",
	"clara-signals": "uid-signals",
	"clara-docs":    "uid-docs",
}

// cand builds a candidate with a resolved identity and optional parents.
func cand(sourceID, local string, parents ...string) recall.Candidate {
	uid, ok := resolver.UID(sourceID)
	if !ok {
		panic("unknown source in test: " + sourceID)
	}
	c := recall.Candidate{
		SourceUID: uid,
		SourceID:  sourceID,
		Locator:   recall.Locator{SourceID: sourceID, SourceUID: uid, Local: local},
	}
	for _, p := range parents {
		loc, err := recall.ParseLocator(p)
		if err != nil {
			panic(err)
		}
		c.DerivedFrom = append(c.DerivedFrom, loc)
	}
	return c
}

func TestResolveFillsMissingIdentity(t *testing.T) {
	byName, err := lineage.Resolve(resolver, recall.Locator{SourceID: "tasks", Local: "td-1"})
	if err != nil {
		t.Fatal(err)
	}
	if byName.SourceUID != "uid-tasks" {
		t.Errorf("uid = %q", byName.SourceUID)
	}

	byUID, err := lineage.Resolve(resolver, recall.Locator{SourceUID: "uid-tasks", Local: "td-1"})
	if err != nil {
		t.Fatal(err)
	}
	if byUID.SourceID != "tasks" {
		t.Errorf("id = %q", byUID.SourceID)
	}
}

func TestResolveUnknownSource(t *testing.T) {
	_, err := lineage.Resolve(resolver, recall.Locator{SourceID: "jira", Local: "PROJ-1"})
	var notConfigured *lineage.ErrNotConfigured
	if !errors.As(err, &notConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// The load-bearing case: a signal projecting a task is the same evidence as
// the task itself, so both must land on one root.
func TestProjectionSharesRootWithOriginal(t *testing.T) {
	task := cand("tasks", "td-f62256")
	signal := cand("clara-signals", "sig-8891", "tasks:td-f62256")

	g := lineage.NewGraph(resolver, []recall.Candidate{task, signal})

	taskLin, err := g.Of(task)
	if err != nil {
		t.Fatal(err)
	}
	signalLin, err := g.Of(signal)
	if err != nil {
		t.Fatal(err)
	}
	if taskLin.Root != signalLin.Root {
		t.Fatalf("roots differ: task %q, signal %q", taskLin.Root, signalLin.Root)
	}
	if want := recall.LineageRoot("uid-tasks:td-f62256"); taskLin.Root != want {
		t.Errorf("root = %q, want %q", taskLin.Root, want)
	}
	if n := lineage.Independent([]lineage.Lineage{taskLin, signalLin}); n != 1 {
		t.Errorf("independent = %d, want 1: a projection never corroborates its original", n)
	}
}

// An edge to a record that was not retrieved still resolves: the upstream
// locator is the original record's identity whether or not it was returned.
func TestEdgeToUnretrievedRecordStillResolves(t *testing.T) {
	signal := cand("clara-signals", "sig-8891", "tasks:td-f62256")
	g := lineage.NewGraph(resolver, []recall.Candidate{signal})

	lin, err := g.Of(signal)
	if err != nil {
		t.Fatal(err)
	}
	if want := recall.LineageRoot("uid-tasks:td-f62256"); lin.Root != want {
		t.Errorf("root = %q, want %q", lin.Root, want)
	}
}

func TestChainFollowsToOriginal(t *testing.T) {
	task := cand("tasks", "td-1")
	signal := cand("clara-signals", "sig-1", "tasks:td-1")
	digest := cand("clara-docs", "digest.md#1", "clara-signals:sig-1")

	g := lineage.NewGraph(resolver, []recall.Candidate{task, signal, digest})
	lin, err := g.Of(digest)
	if err != nil {
		t.Fatal(err)
	}
	if want := recall.LineageRoot("uid-tasks:td-1"); lin.Root != want {
		t.Errorf("root = %q, want %q", lin.Root, want)
	}
	if lin.Depth != 2 {
		t.Errorf("depth = %d, want 2", lin.Depth)
	}
}

// Every member of a cycle must agree on one root or the cycle fails to
// deduplicate. Order of discovery must not change the answer.
func TestCycleConvergesOnOneRoot(t *testing.T) {
	a := cand("tasks", "a", "clara-signals:b")
	b := cand("clara-signals", "b", "tasks:a")

	g := lineage.NewGraph(resolver, []recall.Candidate{a, b})
	linA, err := g.Of(a)
	if err != nil {
		t.Fatal(err)
	}
	linB, err := g.Of(b)
	if err != nil {
		t.Fatal(err)
	}
	if linA.Root != linB.Root {
		t.Fatalf("cycle members disagree: %q vs %q", linA.Root, linB.Root)
	}
	if !linA.Truncated || linA.Reason != "cycle" {
		t.Errorf("cycle not reported: %+v", linA)
	}
}

func TestDepthLimit(t *testing.T) {
	pool := []recall.Candidate{cand("tasks", "n0")}
	for i := 1; i <= lineage.MaxDepth+2; i++ {
		parent := "tasks:n" + string(rune('0'+i-1))
		pool = append(pool, cand("tasks", "n"+string(rune('0'+i)), parent))
	}
	g := lineage.NewGraph(resolver, pool)

	deepest := pool[len(pool)-1]
	lin, err := g.Of(deepest)
	if err != nil {
		t.Fatal(err)
	}
	if !lin.Truncated || lin.Reason != "max_depth" {
		t.Fatalf("expected max_depth truncation, got %+v", lin)
	}
	if lin.Depth > lineage.MaxDepth {
		t.Errorf("followed %d edges, limit is %d", lin.Depth, lineage.MaxDepth)
	}
}

// An unresolvable edge is a fact about this machine's configuration, not a
// query failure. The edge is dropped, reported, and the candidate stands alone.
func TestUnresolvableEdgeIsDroppedNotFatal(t *testing.T) {
	signal := cand("clara-signals", "sig-9", "jira:PROJ-1")
	g := lineage.NewGraph(resolver, []recall.Candidate{signal})

	lin, err := g.Of(signal)
	if err != nil {
		t.Fatalf("unresolvable edge must not fail the query: %v", err)
	}
	if want := recall.LineageRoot("uid-signals:sig-9"); lin.Root != want {
		t.Errorf("root = %q, want the candidate itself %q", lin.Root, want)
	}
	if len(lin.Dropped) != 1 || lin.Dropped[0] != "jira:PROJ-1" {
		t.Errorf("dropped = %v, want the unresolvable edge reported", lin.Dropped)
	}
}

// A candidate with several parents projects no single record, so it keeps its
// own root but must not be counted as evidence independent of its parents.
func TestCompositeIsNotIndependentOfItsParents(t *testing.T) {
	taskA := cand("tasks", "td-1")
	taskB := cand("tasks", "td-2")
	summary := cand("clara-docs", "weekly.md#1", "tasks:td-1", "tasks:td-2")

	g := lineage.NewGraph(resolver, []recall.Candidate{taskA, taskB, summary})
	linSummary, err := g.Of(summary)
	if err != nil {
		t.Fatal(err)
	}
	if want := recall.LineageRoot("uid-docs:weekly.md#1"); linSummary.Root != want {
		t.Errorf("root = %q, want its own %q", linSummary.Root, want)
	}
	if len(linSummary.Ancestors) != 2 {
		t.Fatalf("ancestors = %v, want both parents", linSummary.Ancestors)
	}

	linA, _ := g.Of(taskA)
	linB, _ := g.Of(taskB)

	if n := lineage.Independent([]lineage.Lineage{linA, linB, linSummary}); n != 2 {
		t.Errorf("independent = %d, want 2: the summary restates its parents", n)
	}
	// Alone, the summary is the only evidence there is.
	if n := lineage.Independent([]lineage.Lineage{linSummary}); n != 1 {
		t.Errorf("independent = %d, want 1", n)
	}
}

// A source declaring it projects another cannot say which record it projects,
// so it never changes a root — but it still is not independent evidence.
func TestSourceLevelDerivationSuppressesCorroboration(t *testing.T) {
	task := cand("tasks", "td-1")
	mirror := cand("clara-signals", "mirror-1")

	g := lineage.NewGraph(resolver, []recall.Candidate{task, mirror})
	g.DeclareSourceDerivation("uid-signals", "uid-tasks")

	linMirror, err := g.Of(mirror)
	if err != nil {
		t.Fatal(err)
	}
	if want := recall.LineageRoot("uid-signals:mirror-1"); linMirror.Root != want {
		t.Errorf("root = %q, want unchanged %q", linMirror.Root, want)
	}
	linTask, _ := g.Of(task)

	if n := lineage.Independent([]lineage.Lineage{linTask, linMirror}); n != 1 {
		t.Errorf("independent = %d, want 1: a whole-source projection is not independent", n)
	}
	// Without the upstream source present, the mirror stands on its own.
	if n := lineage.Independent([]lineage.Lineage{linMirror}); n != 1 {
		t.Errorf("independent = %d, want 1", n)
	}
}

// Duplicate roots must collapse before counting, or two hits on one record
// would read as corroboration.
func TestDuplicateRootsCountOnce(t *testing.T) {
	task := cand("tasks", "td-1")
	sameFromElsewhere := cand("clara-signals", "sig-1", "tasks:td-1")
	alsoSame := cand("clara-docs", "note.md#3", "tasks:td-1")

	g := lineage.NewGraph(resolver, []recall.Candidate{task, sameFromElsewhere, alsoSame})
	var lins []lineage.Lineage
	for _, c := range []recall.Candidate{task, sameFromElsewhere, alsoSame} {
		lin, err := g.Of(c)
		if err != nil {
			t.Fatal(err)
		}
		lins = append(lins, lin)
	}
	if n := lineage.Independent(lins); n != 1 {
		t.Errorf("independent = %d, want 1: three views of one record", n)
	}
}
