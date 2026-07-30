package ranking_test

import (
	"testing"

	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/pkg/recall"
)

// Entity matching is where a retrieval layer quietly goes wrong: merge two
// things that are not the same and the answer confidently cites the wrong
// record, with corroboration to back it up. Every pair here is one a substring
// test would have merged. None of them may merge.
func TestEntityMatchingRefusesFalsePositives(t *testing.T) {
	cases := []struct {
		name string
		why  string
		a, b recall.Candidate
	}{
		{
			name: "title is a prefix of the other",
			why:  "the classic substring merge: a spec is not the thing it specifies",
			a:    cand("docs", "a.md#1", 1, title("Recall Spec")),
			b:    cand("mail", "m-1", 1, title("Recall Spec Review Thread")),
		},
		{
			name: "title is a suffix of the other",
			why:  "a qualifier in front changes the subject",
			a:    cand("docs", "a.md#1", 1, title("Release Notes")),
			b:    cand("mail", "m-1", 1, title("Pre Release Notes")),
		},
		{
			name: "personal name extended",
			why:  "two people share a first and last name often enough to matter",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordPerson), title("Marcus Vorwaller")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordPerson), title("Marcus Vorwaller Jr")),
		},
		{
			name: "single word titles",
			why:  "one word is not identity; an adapter that knows better declares an entity id",
			a:    cand("docs", "a.md#1", 1, title("Recall")),
			b:    cand("mail", "m-1", 1, title("Recall")),
		},
		{
			name: "same title, different record types",
			why:  "the meeting and the note about the meeting are different records",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordDocument), title("Weekly Sync")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordEvent), title("Weekly Sync")),
		},
		{
			name: "identifier is a prefix of the other",
			why:  "td-1 and td-12 differ by one character and by everything else",
			a:    cand("docs", "a.md#1", 1, meta(ranking.MetaEntityID, "td-1")),
			b:    cand("mail", "m-1", 1, meta(ranking.MetaEntityID, "td-12")),
		},
		{
			name: "same entity id, different entity type",
			why:  "record identifiers are numbered independently per type",
			a:    cand("docs", "a.md#1", 1, meta(ranking.MetaEntityID, "42"), meta(ranking.MetaEntityType, "person")),
			b:    cand("mail", "m-1", 1, meta(ranking.MetaEntityID, "42"), meta(ranking.MetaEntityType, "task")),
		},
		{
			name: "same record id, different sources",
			why:  "a record id is only unique inside its own source; two sources both number from 1",
			a:    cand("docs", "a.md#1", 1, recordID("42")),
			b:    cand("mail", "m-1", 1, recordID("42")),
		},
		{
			name: "same fingerprint, different record types",
			why:  "identical text in a task and a document is a duplicate to show, not one record",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordDocument), fingerprint("fp-1")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordTask), fingerprint("fp-1")),
		},
		{
			name: "same fingerprint and revision, different record ids",
			why:  "two sources holding one text are still two records; a fingerprint alone is echoable",
			a:    cand("docs", "a.md#1", 1, recordID("42"), fingerprint("fp-1"), revision("rev-1")),
			b:    cand("mail", "m-1", 1, recordID("43"), fingerprint("fp-1"), revision("rev-1")),
		},
		{
			name: "same record id and revision, different fingerprints",
			why:  "two sources numbering from 1 agree on an identifier by accident, not on content",
			a:    cand("docs", "a.md#1", 1, recordID("42"), fingerprint("fp-1"), revision("rev-1")),
			b:    cand("mail", "m-1", 1, recordID("42"), fingerprint("fp-2"), revision("rev-1")),
		},
		{
			name: "same record id and fingerprint, different revisions",
			why:  "one catalog is one revision; disagreeing on it means these were not read together",
			a:    cand("docs", "a.md#1", 1, recordID("42"), fingerprint("fp-1"), revision("rev-1")),
			b:    cand("mail", "m-1", 1, recordID("42"), fingerprint("fp-1"), revision("rev-2")),
		},
		{
			name: "alias shorter than the name",
			why:  "an alias matches a whole name or not at all",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordPerson), title("Marcus Vorwaller")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordPerson), title("Someone Else"), meta(ranking.MetaAliases, []string{"Marcus V"})),
		},
		{
			name: "no identity declared at all",
			why:  "silence is not a match; empty titles and empty fingerprints merge nothing",
			a:    cand("docs", "a.md#1", 1),
			b:    cand("mail", "m-1", 1),
		},
	}

	r := newRanker(t, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Withholding one of the two as a near-duplicate is a display
			// decision and is allowed; treating them as one record is not.
			if clustered(fuse(t, r, request(tc.a, tc.b)), tc.a, tc.b) {
				t.Fatalf("merged into one cluster; must stay separate: %s", tc.why)
			}
		})
	}
}

// clustered reports whether two candidates ended up in the same cluster.
func clustered(f ranking.Fusion, a, b recall.Candidate) bool {
	for _, res := range f.Results {
		var sawA, sawB bool
		for _, m := range res.Members {
			for _, c := range m.Candidates {
				sawA = sawA || c.CandidateID == a.CandidateID
				sawB = sawB || c.CandidateID == b.CandidateID
			}
		}
		if sawA && sawB {
			return true
		}
	}
	return false
}

// The other half of the contract: declared identity must actually merge, or
// clustering is conservative to the point of uselessness.
func TestEntityMatchingMergesDeclaredIdentity(t *testing.T) {
	cases := []struct {
		name string
		why  string
		a, b recall.Candidate
	}{
		{
			name: "same entity id across sources",
			why:  "a typed identifier is the strongest thing an adapter can say",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordPerson), meta(ranking.MetaEntityID, "p-42")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordPerson), meta(ranking.MetaEntityID, "p-42")),
		},
		{
			name: "identical multi-token title and record type",
			why:  "the conservative fallback: whole name, same kind of record",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordPerson), title("Marcus Vorwaller")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordPerson), title("marcus  vorwaller")),
		},
		{
			name: "declared alias equal to the other name",
			why:  "aliases are how a source declares a second full name",
			a:    cand("docs", "a.md#1", 1, kind(recall.RecordPerson), title("Marcus Vorwaller")),
			b:    cand("mail", "m-1", 1, kind(recall.RecordPerson), title("M V"), meta(ranking.MetaAliases, []any{"Marcus Vorwaller"})),
		},
	}

	r := newRanker(t, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fuse(t, r, request(tc.a, tc.b))
			if len(got.Results) != 1 {
				t.Fatalf("results = %v, want one cluster: %s", order(got), tc.why)
			}
		})
	}
}

// A fingerprint collapses corroboration and nothing else.
//
// It deliberately does not cluster: primary selection is by score, and a
// local_rank of 1 is free to whoever is answering, so letting an advisory hash
// merge across sources would let one source capture another's cluster and
// become the record a person is shown. Two sources holding the same text stay
// two results, and stop corroborating each other.
func TestFingerprintCollapsesCorroborationWithoutClustering(t *testing.T) {
	r := newRanker(t, nil)
	got := fuse(t, r, request(
		cand("docs", "a.md#1", 1, fingerprint("fp-1")),
		cand("mail", "m-1", 1, fingerprint("fp-1")),
	))

	if len(got.Results) != 2 {
		t.Fatalf("results = %v, want both records shown and addressable", order(got))
	}
	for _, res := range got.Results {
		if n := res.Explanation.Corroboration.IndependentUnits; n != 1 {
			t.Errorf("independent units = %d, want 1: identical text is one piece of evidence", n)
		}
	}
}

func TestFingerprintMergeKeepsRecordsAddressable(t *testing.T) {
	r := newRanker(t, nil)
	got := single(t, fuse(t, r, request(
		cand("docs", "a.md#1", 1, fingerprint("fp-1"), meta(ranking.MetaEntityID, "e-1")),
		cand("mail", "m-1", 1, fingerprint("fp-1"), meta(ranking.MetaEntityID, "e-1")),
	)))

	if len(got.Members) != 2 {
		t.Errorf("members = %d, want both records expandable", len(got.Members))
	}
	if n := got.Explanation.Corroboration.IndependentUnits; n != 1 {
		t.Errorf("distinct lineages = %d, want 1: the same content is not two opinions", n)
	}
	alone := single(t, fuse(t, r, request(cand("docs", "a.md#1", 1, fingerprint("fp-1")))))
	if got.Score != alone.Score {
		t.Errorf("score = %v, want %v", got.Score, alone.Score)
	}
}

// Two source instances over one index — the same adapter twice, differing by a
// filter — are not two records, and a caller asking one question must not spend
// two of its slots on one project. Record identifier, content fingerprint and
// source revision agreeing at once is what says so.
func TestDuplicateViewsFuseAcrossSourceInstances(t *testing.T) {
	r := newRanker(t, nil)
	view := []opt{recordID("project_5668"), fingerprint("fp-1"), revision("scan-2c6e")}

	// The better-ranked view of the record; the other trails it by a rank.
	better := cand("docs", "project_5668", 1, view...)
	worse := cand("mail", "project_5668", 2, view...)

	got := fuse(t, r, request(better, worse))
	res := single(t, got)

	if res.Primary.CandidateID != better.CandidateID {
		t.Errorf("primary = %s, want the better-ranked view %s",
			res.Primary.CandidateID, better.CandidateID)
	}
	// Both roots stay: each is a true account of where the record was read, and
	// either locator expands.
	if len(res.Members) != 2 {
		t.Errorf("members = %d, want both views expandable", len(res.Members))
	}
	// Fusing changes what is displayed, never what corroborates: a fingerprint
	// already collapsed these into one unit and must not now count two.
	if n := res.Explanation.Corroboration.IndependentUnits; n != 1 {
		t.Errorf("independent units = %d, want 1: one record read twice", n)
	}
	alone := single(t, fuse(t, r, request(better)))
	if res.Score != alone.Score {
		t.Errorf("score = %v, want exactly %v: a second view is not more evidence",
			res.Score, alone.Score)
	}

	// The view that did not take the slot is reported rather than vanishing,
	// and names the result it was folded into so a reader can tell a record
	// they still have from one they were denied.
	want := recall.Suppression{
		Reason:      recall.SuppressDuplicateView,
		Count:       1,
		LineageRoot: recall.LineageRoot("uid-mail:project_5668"),
		FusedInto:   recall.LineageRoot("uid-docs:project_5668"),
	}
	if len(got.Suppressed) != 1 || got.Suppressed[0] != want {
		t.Errorf("suppressed = %+v, want %+v", got.Suppressed, want)
	}
}

// The count is result slots, not candidates. A view that arrived as a record
// and a projection of it was always going to be one result, so folding it
// costs the answer one slot and reporting two would overstate what is missing.
func TestDuplicateViewCountsSlotsNotCandidates(t *testing.T) {
	r := newRanker(t, nil)
	view := []opt{recordID("project_5668"), fingerprint("fp-1"), revision("scan-2c6e")}

	got := fuse(t, r, request(
		cand("docs", "project_5668", 1, view...),
		cand("mail", "project_5668", 2, view...),
		// A third candidate projecting the weaker view: one more candidate in
		// that lineage group, still the same one record.
		cand("signals", "sig-1", 9, from("mail:project_5668")),
	))
	single(t, got)

	if len(got.Suppressed) != 1 {
		t.Fatalf("suppressed = %+v, want one withheld view", got.Suppressed)
	}
	if n := got.Suppressed[0].Count; n != 1 {
		t.Errorf("count = %d, want 1: two candidates of one record cost one result slot", n)
	}
}

// Pre-reply suppression is keyed on what the host has already shown, and a
// fused cluster is one record under two roots. Naming either root has shown
// the record, so the cluster must not come back under the other one.
func TestDuplicateViewDoesNotDefeatLineageSuppression(t *testing.T) {
	r := newRanker(t, nil)
	view := []opt{recordID("project_5668"), fingerprint("fp-1"), revision("scan-2c6e")}
	better := cand("docs", "project_5668", 1, view...)
	worse := cand("mail", "project_5668", 2, view...)

	// The second is the reviewer's case: the host suppressed the view that
	// wins the slot, and the other view's root is the one nobody named.
	for _, shown := range []recall.LineageRoot{"uid-mail:project_5668", "uid-docs:project_5668"} {
		t.Run(string(shown), func(t *testing.T) {
			req := request(better, worse)
			req.Mode = recall.ModePreReply
			req.SuppressLineages = []recall.LineageRoot{shown}

			got := fuse(t, r, req)
			if len(got.Results) != 0 {
				t.Fatalf("results = %v, want none: the host has already been shown this record",
					order(got))
			}
			if len(got.Suppressed) != 1 || got.Suppressed[0].Reason != recall.SuppressLineageSeen {
				t.Errorf("suppressed = %+v, want one %s", got.Suppressed, recall.SuppressLineageSeen)
			}
		})
	}
}

// Which view is shown is a function of the groups, not of the order the
// adapters answered in.
func TestDuplicateViewWinnerIsIndependentOfArrivalOrder(t *testing.T) {
	r := newRanker(t, nil)
	view := []opt{recordID("project_5668"), fingerprint("fp-1"), revision("scan-2c6e")}
	better := cand("docs", "project_5668", 1, view...)
	worse := cand("mail", "project_5668", 2, view...)

	forward := single(t, fuse(t, r, request(better, worse)))
	reverse := single(t, fuse(t, r, request(worse, better)))
	if forward.Primary.CandidateID != reverse.Primary.CandidateID {
		t.Errorf("primary = %s reversed, %s forward",
			reverse.Primary.CandidateID, forward.Primary.CandidateID)
	}

	// Equal ranks leave score unable to decide, and the tie breaks on the root
	// rather than on arrival.
	tied := []opt{recordID("project_5668"), fingerprint("fp-2"), revision("scan-2c6e")}
	a := cand("docs", "project_5668", 1, tied...)
	b := cand("mail", "project_5668", 1, tied...)
	first := single(t, fuse(t, r, request(a, b)))
	second := single(t, fuse(t, r, request(b, a)))
	if first.Primary.CandidateID != a.CandidateID || second.Primary.CandidateID != a.CandidateID {
		t.Errorf("primary = %s and %s, want %s both times",
			first.Primary.CandidateID, second.Primary.CandidateID, a.CandidateID)
	}
}

// Two sources over ONE store are one piece of evidence, whatever either of them
// hashed.
//
// This is the live defect it was written for: a lexical document source and a
// semantic one configured over the same directory reported "corroborated 2" on
// every document they both returned — a doubled score on precisely the best
// answers, presented to the caller as independent evidence. Neither existing
// rule can see it. duplicateKeys is scoped by source, correctly. fingerprintKeys
// needs both sides to hash the same text, and two document backends chunk
// differently by design, so their fingerprints and revisions are structurally
// unable to match.
//
// The titles AGREE here, and that is load-bearing rather than incidental: the
// name relation is the only thing that puts the two groups in one cluster at
// all, so a version of this test whose titles differ passes while the defect
// stands. In the live case the two adapters agreed because the retrieved chunk
// was the document's own H1.
func TestTwoSourcesOverOneLocationDoNotCorroborate(t *testing.T) {
	r := newRanker(t, nil)
	// Same record, same title, deliberately different content hashes and source
	// revisions — which is what two chunkers over one file really produce.
	pair := []recall.Candidate{
		cand("docs", "notes/tooth-care.md#L1-L7", 1,
			title("Tooth care appointment"), recordID("notes/tooth-care.md"),
			fingerprint("sha256:docs-chunk"), revision("git:abc+fs:def")),
		cand("mail", "notes/tooth-care.md#L1-L4", 1,
			title("Tooth care appointment"), recordID("notes/tooth-care.md"),
			fingerprint("qmd-docid:43f92c"), revision("collection=x files=5")),
	}

	// Without the location map nothing links them: this is the defect, and it is
	// asserted so that the fix cannot be mistaken for something the other rules
	// were already doing.
	inflated := single(t, fuse(t, r, request(pair...)))
	if n := inflated.Explanation.Corroboration.IndependentUnits; n != 2 {
		t.Fatalf("independent units = %d without locations; the case no longer "+
			"reproduces the defect and proves nothing", n)
	}

	req := request(pair...)
	req.SourceLocations = map[recall.SourceUID]string{
		"uid-docs": "/store/corpus",
		"uid-mail": "/store/corpus",
	}
	got := single(t, fuse(t, r, req))
	if n := got.Explanation.Corroboration.IndependentUnits; n != 1 {
		t.Errorf("independent units = %d, want 1: two views of one file are one "+
			"piece of evidence", n)
	}
	if got.Score >= inflated.Score {
		t.Errorf("score = %v, want below the inflated %v", got.Score, inflated.Score)
	}
	// Both records stay addressable. The rule lowers a score; it does not decide
	// which source a caller is shown.
	if len(got.Members) != 2 {
		t.Errorf("members = %d, want both records expandable", len(got.Members))
	}
}

// The rule is corroboration-only and must never reach display. A source is free
// to put local_rank 1 on anything, so a rule that merged for display would let
// the echoing source capture the cluster and demote the honest evidence to a
// member of it.
func TestOneLocationDoesNotMergeForDisplay(t *testing.T) {
	r := newRanker(t, nil)
	// No title agreement, so nothing else can cluster them either.
	req := request(
		cand("docs", "a.md#L1-L3", 1, title("Quartz handbook"), recordID("a.md")),
		cand("mail", "a.md#L1-L9", 1, title("Something else entirely"), recordID("a.md")),
	)
	req.SourceLocations = map[recall.SourceUID]string{
		"uid-docs": "/store/corpus",
		"uid-mail": "/store/corpus",
	}
	got := fuse(t, r, req)
	if len(got.Results) != 2 {
		t.Fatalf("results = %v, want both records shown and addressable", order(got))
	}
	for _, res := range got.Results {
		if n := res.Explanation.Corroboration.IndependentUnits; n != 1 {
			t.Errorf("independent units = %d, want 1", n)
		}
	}
}

// Two sources over one location serving different record types are not
// restating each other, so the key is scoped by record type exactly as a
// fingerprint is.
func TestOneLocationScopesByRecordType(t *testing.T) {
	r := newRanker(t, nil)
	// Clustered by a declared entity so the two really do meet inside one
	// cluster: without that they would be two clusters for an unrelated reason
	// and the record-type scoping would go untested.
	req := request(
		cand("docs", "a.md#L1-L3", 1, recordID("a.md"),
			meta(ranking.MetaEntityID, "e-1"), meta(ranking.MetaEntityType, "shared")),
		cand("mail", "a.md#L1-L3", 1, recordID("a.md"),
			meta(ranking.MetaEntityID, "e-1"), meta(ranking.MetaEntityType, "shared"),
			func(c *recall.Candidate) { c.RecordType = recall.RecordEvent }),
	)
	req.SourceLocations = map[recall.SourceUID]string{
		"uid-docs": "/store/corpus",
		"uid-mail": "/store/corpus",
	}
	got := single(t, fuse(t, r, req))
	if n := got.Explanation.Corroboration.IndependentUnits; n != 2 {
		t.Errorf("independent units = %d, want 2: a document and an event are "+
			"not two views of one record", n)
	}
}

// Different locations are different stores, and a source with no location makes
// no claim about one. Both must leave every existing profile exactly as it was.
func TestLocationKeyIsEmptySafe(t *testing.T) {
	r := newRanker(t, nil)
	pair := []recall.Candidate{
		cand("docs", "a.md#L1-L3", 1, title("Weekly review"), recordID("a.md")),
		cand("mail", "a.md#L1-L4", 1, title("Weekly review"), recordID("a.md")),
	}
	for name, locations := range map[string]map[recall.SourceUID]string{
		"no map":         nil,
		"empty map":      {},
		"one absent":     {"uid-docs": "/store/corpus"},
		"both absent":    {"uid-tasks": "/elsewhere"},
		"different dirs": {"uid-docs": "/store/corpus", "uid-mail": "/store/other"},
		"empty strings":  {"uid-docs": "", "uid-mail": ""},
	} {
		t.Run(name, func(t *testing.T) {
			req := request(pair...)
			req.SourceLocations = locations
			got := single(t, fuse(t, r, req))
			if n := got.Explanation.Corroboration.IndependentUnits; n != 2 {
				t.Errorf("independent units = %d, want 2: nothing established a "+
					"shared store", n)
			}
		})
	}
}
