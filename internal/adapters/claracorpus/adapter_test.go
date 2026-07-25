package claracorpus_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/adapters/claracorpus"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// probeTime is the instant every test's clock reports, so the decay arithmetic
// under test is arithmetic and not a function of the day the suite runs.
var probeTime = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

const (
	// One live signal and one archived one, from the store the Tasks source
	// also owns, plus a Slack DM whose excerpt is somebody else's words.
	signalsLive = `{"type":"signal","schema_version":2,"id":"s0000001","source":"tasks","kind":"todo","ref":"tasks:aa11bb22","source_id":"aa11bb22","content_trust":"untrusted","title":"Renew the studio insurance","status":"TODO","occurred_at":"2026-03-01T09:00:00Z","first_seen":"2026-03-01","last_seen":"2026-03-03","run_count":3,"lifecycle_state":"active","summary":"TODO — Renew the studio insurance","raw_excerpt":"@admin"}
{"type":"signal","schema_version":2,"id":"s0000002","source":"slack","kind":"dm","ref":"slack:C0AAA:1740926400.000100","source_id":"1740926400.000100","content_trust":"untrusted","title":"Dana asked about insurance","occurred_at":"2026-03-02T14:20:00Z","first_seen":"2026-03-02","last_seen":"2026-03-03","run_count":1,"lifecycle_state":"active","summary":"Dana asked when insurance renews","raw_excerpt":"when does it renew?"}
{"type":"signal","schema_version":2,"id":"s0000003","source":"reddit","kind":"post","ref":"reddit:xyz789","source_id":"xyz789","content_trust":"untrusted","title":"Unmapped source insurance thread","occurred_at":"2026-03-02T15:00:00Z","first_seen":"2026-03-02","last_seen":"2026-03-03","run_count":1,"lifecycle_state":"active","summary":"nothing maps this source"}
`
	signalsArchive = `{"type":"signal","schema_version":2,"id":"s0000004","source":"tasks","kind":"todo","ref":"tasks:77aa88bb","source_id":"77aa88bb","content_trust":"untrusted","title":"Compare insurance quotes","status":"DONE","occurred_at":"2026-01-09T10:00:00Z","first_seen":"2026-01-09","last_seen":"2026-01-30","run_count":9,"lifecycle_state":"inactive","inactive_reason":"handled","inactive_at":"2026-01-30","archived_at":"2026-02-06","summary":"DONE — Compare insurance quotes"}
`
	observations = `{"type":"observation","schema_version":2,"id":"ob000001","ref":"tasks:aa11bb22","signal_id":"s0000001","action":"dismissed","source":"tasks","kind":"todo","occurred_at":"2026-03-02T18:00:00Z","metadata":{}}
{"type":"observation","schema_version":2,"id":"ob000002","ref":"tasks:aa11bb22","signal_id":"s0000001","action":"acted","source":"tasks","kind":"todo","occurred_at":"2026-03-03T09:15:00Z","metadata":{}}
`
	// A fact that never decays, a preference that has faded, and the generated
	// preference whose provenance makes it a composite.
	memoryLive = `{"type":"memory","schema_version":2,"id":"m0000001","kind":"fact","subject":"studio-insurance","title":"Insurance renews in March","body":"The studio policy renews on 4 March.","weight":1.0,"created":"2025-03-05","last_seen":"2026-03-03","hits":4,"tags":["admin"],"source":"manual"}
{"type":"memory","schema_version":2,"id":"m0000002","kind":"preference","subject":"insurance-quotes","title":"Compare insurance quotes first","body":"Compare at least two quotes before renewing.","weight":0.7,"half_life_days":45,"created":"2026-01-30","last_seen":"2026-01-30","hits":1,"source":"observed"}
{"type":"memory","schema_version":2,"id":"m0000003","kind":"preference","subject":"pref:observed:tasks:todo","title":"Acts on tasks todo signals","body":"Acts rather than dismissing.","weight":0.7,"half_life_days":45,"created":"2026-03-03","last_seen":"2026-03-03","hits":4,"tags":["generated-preference","tasks","todo"],"source":"observed","effect":{"source":"tasks","kind":"todo","direction":1},"provenance":{"type":"observations-v1","threshold":4,"observation_ids":["ob1","ob2","ob3","ob4"],"refs":["tasks:aa11bb22","tasks:77aa88bb","tasks:cc33dd44","tasks:aa11bb22"],"actions":["acted","acted","acted","acted"]}}
`
	memoryArchive = `{"type":"memory","schema_version":2,"id":"m0000004","kind":"preference","subject":"insurance-quotes","title":"Compare insurance quotes first","body":"Asks for three quotes before renewing.","weight":0.5,"half_life_days":45,"created":"2024-06-01","last_seen":"2024-08-01","hits":2,"source":"observed"}
`
	stateJSON = `{"last_successful_run_id":"5c0ffee0","lookback_since":"2026-03-01"}
`
)

// corpus writes a Clara data directory holding the named files.
func corpus(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := files["state.json"]; !ok {
		files["state.json"] = stateJSON
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func signalCorpus(t *testing.T) string {
	t.Helper()
	return corpus(t, map[string]string{
		"signals.jsonl":         signalsLive,
		"signals-archive.jsonl": signalsArchive,
		"observations.jsonl":    observations,
	})
}

func memoryCorpus(t *testing.T) string {
	t.Helper()
	return corpus(t, map[string]string{
		"memory.jsonl":         memoryLive,
		"memory-archive.jsonl": memoryArchive,
	})
}

// start builds an initialized adapter over dir. Settings are merged over the
// defaults for the store so a test states only what it is about.
func start(t *testing.T, dir, sourceID string, settings map[string]any) (*claracorpus.Adapter, recall.Manifest) {
	t.Helper()
	a := claracorpus.New(claracorpus.Options{Clock: func() time.Time { return probeTime }})
	m, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: sourceID, Location: dir,
		Settings: settings,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, m
}

func signalSettings(extra ...map[string]any) map[string]any {
	s := map[string]any{
		"store":    "signals",
		"upstream": map[string]any{"tasks": "tasks", "slack": "slack-dms"},
	}
	return merge(s, extra...)
}

func memorySettings(extra ...map[string]any) map[string]any {
	s := map[string]any{
		"store":       "memory",
		"upstream":    map[string]any{"tasks": "tasks"},
		"debug_today": "2026-03-10",
	}
	return merge(s, extra...)
}

func merge(base map[string]any, extra ...map[string]any) map[string]any {
	for _, e := range extra {
		for k, v := range e {
			base[k] = v
		}
	}
	return base
}

func search(t *testing.T, a *claracorpus.Adapter, req recall.SearchRequest) recall.SearchResponse {
	t.Helper()
	if req.Deadline.IsZero() {
		req.Deadline = probeTime.Add(time.Hour)
	}
	if req.Limit == 0 {
		req.Limit = 25
	}
	res, err := a.Search(t.Context(), req)
	if err != nil {
		t.Fatalf("search %q: %v", req.Query, err)
	}
	return res
}

func ids(res recall.SearchResponse) []string {
	out := make([]string, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		out = append(out, c.SourceRecordID)
	}
	return out
}

func find(t *testing.T, res recall.SearchResponse, id string) recall.Candidate {
	t.Helper()
	for _, c := range res.Candidates {
		if c.SourceRecordID == id {
			return c
		}
	}
	t.Fatalf("candidate %s not among %v", id, ids(res))
	return recall.Candidate{}
}

// --- settings and location ---------------------------------------------------

func TestStoreIsRequiredAndClosed(t *testing.T) {
	dir := signalCorpus(t)
	for name, settings := range map[string]map[string]any{
		"absent":  {},
		"empty":   {"store": ""},
		"unknown": {"store": "run-manifests"},
	} {
		t.Run(name, func(t *testing.T) {
			a := claracorpus.New(claracorpus.Options{})
			_, err := a.Initialize(t.Context(), adapter.Config{
				ProtocolVersionMin: 1, ProtocolVersionMax: 1,
				Workdir: t.TempDir(), SourceID: "x", Location: dir, Settings: settings,
			})
			if err == nil {
				t.Fatal("a store nobody named was accepted; it would answer out of whichever file it guessed")
			}
			if !strings.Contains(err.Error(), "store must be one of") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

func TestSettingsRefusals(t *testing.T) {
	dir := signalCorpus(t)
	cases := map[string]map[string]any{
		"unknown key":              {"store": "signals", "stores": "signals"},
		"upstream with separator":  {"store": "signals", "upstream": map[string]any{"tasks": "tasks:live"}},
		"upstream with empty name": {"store": "signals", "upstream": map[string]any{"tasks": ""}},
		"unknown timezone":         {"store": "memory", "timezone": "Mars/Olympus"},
		"negative candidates":      {"store": "signals", "max_candidates": -1},
		"today on signals":         {"store": "signals", "debug_today": "2026-03-10"},
		"today not a date":         {"store": "memory", "debug_today": "yesterday"},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			a := claracorpus.New(claracorpus.Options{})
			if _, err := a.Initialize(t.Context(), adapter.Config{
				ProtocolVersionMin: 1, ProtocolVersionMax: 1,
				Workdir: t.TempDir(), SourceID: "x", Location: dir, Settings: settings,
			}); err == nil {
				t.Fatal("accepted, so it is configuration with no code path behind it")
			}
		})
	}
}

func TestVersionRangeAboveThisBuildFailsTheHandshake(t *testing.T) {
	a := claracorpus.New(claracorpus.Options{})
	_, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 2, ProtocolVersionMax: 3,
		Workdir: t.TempDir(), SourceID: "x", Location: signalCorpus(t),
		Settings: signalSettings(),
	})
	if err == nil {
		t.Fatal("a range this build cannot speak was negotiated anyway")
	}
}

// TestLocationResolvesToOneStoreHoweverItIsSpelled is the whole point of
// store_identity: two spellings of one corpus must not look like two stores.
func TestLocationResolvesToOneStoreHoweverItIsSpelled(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "signals.jsonl"), []byte(signalsLive), 0o600); err != nil {
		t.Fatal(err)
	}

	byRoot, _ := start(t, root, "a", signalSettings())
	byData, _ := start(t, data, "b", signalSettings())
	byTrailing, _ := start(t, data+string(filepath.Separator), "c", signalSettings())

	first := storeIdentity(t, byRoot)
	for _, other := range []*claracorpus.Adapter{byData, byTrailing} {
		if got := storeIdentity(t, other); got != first {
			t.Errorf("store identity %q != %q: two spellings of one corpus would pass doctor's isolation check", got, first)
		}
	}
}

func TestTheTwoStoresAreNotOneStore(t *testing.T) {
	dir := corpus(t, map[string]string{
		"signals.jsonl": signalsLive,
		"memory.jsonl":  memoryLive,
	})
	signals, _ := start(t, dir, "clara-signals", signalSettings())
	memory, _ := start(t, dir, "clara-memory", memorySettings())
	if storeIdentity(t, signals) == storeIdentity(t, memory) {
		t.Fatal("the signals and memory instances claim one store, so doctor would refuse a correct profile")
	}
}

func storeIdentity(t *testing.T, a *claracorpus.Adapter) string {
	t.Helper()
	h, err := a.Health(t.Context())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	id, _ := h.Diagnostics[protocol.DiagStoreIdentity].(string)
	if id == "" {
		t.Fatal("no store_identity: this adapter claims exclusivity and has to say over what")
	}
	return id
}

func TestADirectoryThatIsNotACorpusIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := claracorpus.New(claracorpus.Options{})
	_, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "x", Location: dir, Settings: signalSettings(),
	})
	if err == nil {
		t.Fatal("a mistyped path became a source reporting complete coverage over nothing")
	}
	if !strings.Contains(err.Error(), "no Clara store files") {
		t.Errorf("error = %v", err)
	}
}

// --- coverage honesty --------------------------------------------------------

// TestAnAbsentOptionalStoreIsCompleteNotPartial holds the line the other way:
// over-reporting partial would make a correctly configured corpus permanently
// degraded and the signal would stop meaning anything.
func TestAnAbsentOptionalStoreIsCompleteNotPartial(t *testing.T) {
	dir := corpus(t, map[string]string{"signals.jsonl": signalsLive})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	if res.Outcome != recall.SearchSuccess {
		t.Errorf("outcome = %s: nothing has been archived or acted on, which is not a partial read", res.Outcome)
	}
	h, err := a.Health(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != recall.HealthHealthy || h.Coverage != recall.IndexComplete {
		t.Errorf("health = %s/%s", h.Status, h.Coverage)
	}
	absent, _ := h.Diagnostics["absent_files"].([]string)
	if len(absent) != 2 {
		t.Errorf("absent_files = %v, want both the archive and the observation log named", absent)
	}
}

func TestAnUnparseableRecordReportsPartial(t *testing.T) {
	dir := corpus(t, map[string]string{
		"signals.jsonl": signalsLive +
			`{"type":"signal","schema_version":9,"id":"s0000009","source":"tasks","ref":"tasks:future","title":"newer Clara"}` + "\n" +
			"not json at all\n",
	})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	if res.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %s: a record that failed to parse is unknown, not absent", res.Outcome)
	}
	if got := res.Diagnostics["failed_records"]; got != 2 {
		t.Errorf("failed_records = %v, want 2", got)
	}
	h, _ := a.Health(t.Context())
	if h.Status != recall.HealthDegraded || h.Coverage != recall.IndexPartial {
		t.Errorf("health = %s/%s, want degraded/partial", h.Status, h.Coverage)
	}
	for _, candidate := range res.Candidates {
		if candidate.ObservedAt == nil {
			t.Errorf("%s was not observed", candidate.SourceRecordID)
		}
		if candidate.ConfirmedAt != nil {
			t.Errorf("%s confirmed_at = %v on a partial scan", candidate.SourceRecordID, candidate.ConfirmedAt)
		}
	}
}

func TestAnOversizeLineIsDiscardedWithoutLosingTheBoundedRemainder(t *testing.T) {
	hostile := `{"type":"signal","schema_version":2,"id":"huge","source":"tasks","ref":"tasks:huge","title":"` +
		strings.Repeat("x", 4<<20) + `"}` + "\n"
	dir := corpus(t, map[string]string{"signals.jsonl": hostile + signalsLive})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	if res.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %s, want partial for the bounded-away line", res.Outcome)
	}
	if got := res.Diagnostics["failed_records"]; got != 1 {
		t.Errorf("failed_records = %v, want 1", got)
	}
	if len(res.Candidates) != 3 {
		t.Errorf("candidates = %v: records after the malicious line were lost", ids(res))
	}
}

// TestALostObservationIsPartialToo is the coverage boundary this store owns.
// The signals are all present; what is unknown is what the owner did about one,
// and a signal shown without an action it has reads as an open item.
func TestALostObservationIsPartialToo(t *testing.T) {
	dir := corpus(t, map[string]string{
		"signals.jsonl":      signalsLive,
		"observations.jsonl": observations + `{"type":"observation","schema_version":2,"id":"ob9","ref":"tasks:aa11bb22","action":"acted"}` + "\n",
	})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	if res.Outcome != recall.SearchPartial {
		t.Fatalf("outcome = %s", res.Outcome)
	}
	if got := res.Diagnostics["failed_observation_records"]; got != 1 {
		t.Errorf("failed_observation_records = %v, want 1", got)
	}
	if _, reported := res.Diagnostics["failed_records"]; reported {
		t.Error("a lost observation was counted as a lost signal; they are different problems")
	}
}

func TestAnUnreadableStoreIsNeverAnEmptyOne(t *testing.T) {
	dir := corpus(t, map[string]string{"memory.jsonl": memoryLive})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	// The corpus vanishes under a running instance.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	res, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "insurance", Limit: 10, Deadline: probeTime.Add(time.Hour),
	})
	if err == nil {
		t.Fatal("a corpus that is gone answered as a corpus with no matches")
	}
	if res.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %s", res.Outcome)
	}
	h, _ := a.Health(t.Context())
	if h.Status != recall.HealthUnavailable || h.Coverage != recall.IndexUnknown {
		t.Errorf("health = %s/%s, want unavailable/unknown", h.Status, h.Coverage)
	}
	if h.Diagnostics[protocol.DiagStoreIdentity] == nil {
		t.Error("an unreachable instance stopped naming the store it owns, so a collision would be half-visible")
	}
}

// --- lineage -----------------------------------------------------------------

func TestASignalAndItsTaskAreOneLineageRoot(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})

	c := find(t, res, "s0000001")
	if len(c.DerivedFrom) != 1 {
		t.Fatalf("derived_from = %v, want one edge", c.DerivedFrom)
	}
	// Character-for-character the locator internal/adapters/tasks writes: its
	// local part is the task's own id.
	if got := c.DerivedFrom[0].String(); got != "tasks:aa11bb22" {
		t.Errorf("edge = %q, want tasks:aa11bb22", got)
	}
}

func TestAnUnmappedSourceEmitsNoEdge(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})

	if c := find(t, res, "s0000003"); len(c.DerivedFrom) != 0 {
		t.Errorf("derived_from = %v: an invented source_id resolves somewhere, and a wrong root is worse than none",
			c.DerivedFrom)
	}
	if c := find(t, res, "s0000002"); c.DerivedFrom[0].String() != "slack-dms:1740926400.000100" {
		t.Errorf("edge = %v", c.DerivedFrom)
	}
}

func TestAGeneratedPreferenceIsAComposite(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{Query: "acts"})

	c := find(t, res, "m0000003")
	if len(c.DerivedFrom) != 3 {
		t.Fatalf("derived_from = %v, want one edge per distinct provenance ref", c.DerivedFrom)
	}
	for _, edge := range c.DerivedFrom {
		if !strings.HasPrefix(edge.String(), "tasks:") {
			t.Errorf("edge %q does not name the mapped upstream source", edge)
		}
	}
	if c.Metadata["generated_preference"] != true {
		t.Error("the composite does not say it is one")
	}
}

func TestAWrittenMemoryDeclaresNoAncestors(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	if c := find(t, res, "m0000001"); len(c.DerivedFrom) != 0 {
		t.Errorf("derived_from = %v: a fact somebody wrote projects nothing", c.DerivedFrom)
	}
}

func TestAnUnmappedGeneratedPreferenceEmitsNoEdge(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory",
		merge(memorySettings(), map[string]any{"upstream": map[string]any{}}))
	res := search(t, a, recall.SearchRequest{Query: "acts"})
	if c := find(t, res, "m0000003"); len(c.DerivedFrom) != 0 {
		t.Errorf("derived_from = %v with nothing mapped", c.DerivedFrom)
	}
}

// --- observations ------------------------------------------------------------

func TestObservationsProjectOntoSignalsAndAreNeverCandidates(t *testing.T) {
	a, m := start(t, signalCorpus(t), "clara-signals", signalSettings())
	for _, rt := range m.RecordTypes {
		if rt == "observation" {
			t.Fatal("the manifest offers an observation record type; observations are a projection")
		}
	}

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	for _, c := range res.Candidates {
		if strings.HasPrefix(c.SourceRecordID, "ob") {
			t.Fatalf("observation %s became a candidate", c.SourceRecordID)
		}
	}

	c := find(t, res, "s0000001")
	// Latest per ref by (occurred_at, id), exactly as Clara projects it: the
	// earlier "dismissed" lost to the later "acted".
	if c.Metadata["last_action"] != "acted" {
		t.Errorf("last_action = %v, want acted", c.Metadata["last_action"])
	}
	if c.Metadata["action_count"] != 2 {
		t.Errorf("action_count = %v, want 2", c.Metadata["action_count"])
	}
	if c.Metadata["last_action_at"] != "2026-03-03T09:15:00Z" {
		t.Errorf("last_action_at = %v", c.Metadata["last_action_at"])
	}
}

func TestReactionHistoryIsReachableThroughContextExpansion(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "clara-signals", Local: "sig/v2/s0000001"},
		Detail:  recall.DetailContext, Budget: 4096, Deadline: probeTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, want := range []string{"Reactions:", "dismissed", "acted", "2026-03-02T18:00:00Z"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("context expansion is missing %q:\n%s", want, res.Content)
		}
	}
}

// --- decay -------------------------------------------------------------------

// TestDecayIsClarasArithmetic checks the formula against values computed from
// Clara's own expression, including the two cases where a second decay model
// would show up: a null half-life, which Clara does not decay at all, and a
// record stamped in the future, which Clara floors at zero rather than
// reinforcing by arithmetic.
func TestDecayIsClarasArithmetic(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})

	cases := []struct {
		id        string
		weight    float64
		halfLife  float64
		hasHL     bool
		ageDays   int
		effective float64
	}{
		{id: "m0000001", weight: 1.0, ageDays: 7, effective: 1.0},
		{id: "m0000002", weight: 0.7, halfLife: 45, hasHL: true, ageDays: 39,
			effective: 0.7 * math.Pow(0.5, 39.0/45.0)},
		{id: "m0000004", weight: 0.5, halfLife: 45, hasHL: true, ageDays: 586,
			effective: 0.5 * math.Pow(0.5, 586.0/45.0)},
	}
	for _, tc := range cases {
		c := find(t, res, tc.id)
		if got := c.Metadata["age_days"]; got != tc.ageDays {
			t.Errorf("%s age_days = %v, want %d", tc.id, got, tc.ageDays)
		}
		if got := c.Metadata["weight"]; got != tc.weight {
			t.Errorf("%s weight = %v, want %v", tc.id, got, tc.weight)
		}
		hl, present := c.Metadata["half_life_days"]
		if present != tc.hasHL {
			t.Errorf("%s half_life_days present = %v, want %v", tc.id, present, tc.hasHL)
		}
		if tc.hasHL && hl != tc.halfLife {
			t.Errorf("%s half_life_days = %v, want %v", tc.id, hl, tc.halfLife)
		}
		got, _ := c.Metadata["effective_weight"].(float64)
		if math.Abs(got-tc.effective) > 5e-5 {
			t.Errorf("%s effective_weight = %v, want %v", tc.id, got, tc.effective)
		}
		if c.Metadata["decay_basis"] != "last_seen" {
			t.Errorf("%s aged from %v, and Clara ages from last_seen", tc.id, c.Metadata["decay_basis"])
		}
		if _, ok := c.Metadata["decay"].(string); !ok {
			t.Errorf("%s carries no explanation, so its effective weight cannot be checked", tc.id)
		}
	}
}

// TestANullHalfLifeIsNotAnInvitationToUseTheWriteTimeDefault is the sharpest
// decay rule: Record::DEFAULT_HALF_LIFE is consulted when a record is created
// and never again, so applying it here would decay exactly the records Clara
// decided should not decay.
func TestANullHalfLifeIsNotAnInvitationToUseTheWriteTimeDefault(t *testing.T) {
	dir := corpus(t, map[string]string{
		"memory.jsonl": `{"type":"memory","schema_version":2,"id":"m0000010","kind":"preference","subject":"durable","title":"A preference with no half life","body":"kept on purpose","weight":0.7,"created":"2020-01-01","last_seen":"2020-01-01","hits":1,"source":"manual"}` + "\n",
	})
	a, _ := start(t, dir, "clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{Query: "preference"})

	c := find(t, res, "m0000010")
	if got, _ := c.Metadata["effective_weight"].(float64); got != 0.7 {
		t.Errorf("effective_weight = %v after six years, want the weight unchanged", got)
	}
	if _, present := c.Metadata["half_life_days"]; present {
		t.Error("a half-life was invented for a record whose half_life_days is null")
	}
}

func TestAFadedRecordIsDemotedAndStillRetrievable(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})

	got := ids(res)
	want := []string{"m0000001", "m0000002", "m0000004"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want all three retrievable", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v: live before archived, and the fresher preference above the faded one", got, want)
		}
	}
	if find(t, res, "m0000004").Metadata["standing"] != "archived" {
		t.Error("the archived record does not say it is archived")
	}
}

// TestATimeWindowSuspendsTheDecayPenalty is spec.md#decay's rule that a
// historical query retrieves old evidence without a recency penalty. The same
// query without a window ranks m0000001 first; with one, the two swap, because
// only decay was separating them.
func TestATimeWindowSuspendsTheDecayPenalty(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	windowed := search(t, a, recall.SearchRequest{
		Query: "insurance", Filters: recall.Filters{Since: &since, Until: &until},
	})
	if got := ids(windowed)[0]; got != "m0000002" {
		t.Errorf("first = %s, want m0000002: with decay suspended the newer record wins on event time", got)
	}
	if windowed.Diagnostics["decay_applied"] != false {
		t.Error("a windowed search did not report that decay was suspended")
	}
	plain := search(t, a, recall.SearchRequest{Query: "insurance"})
	if got := ids(plain)[0]; got != "m0000001" {
		t.Errorf("first = %s without a window, want m0000001", got)
	}
}

// TestSignalsGetNoAgeArithmetic holds the other half of the decay decision:
// Clara's lifecycle already decided, and recomputing an expiry here would be a
// second model.
func TestSignalsGetNoAgeArithmetic(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	for _, c := range res.Candidates {
		for _, key := range []string{"effective_weight", "half_life_days", "age_days", "decay"} {
			if _, present := c.Metadata[key]; present {
				t.Errorf("signal %s carries %s; its fading is Clara's lifecycle, not arithmetic", c.SourceRecordID, key)
			}
		}
	}
	if find(t, res, "s0000004").Metadata["standing"] != "archived" {
		t.Error("the archived signal does not carry Clara's verdict")
	}
}

func TestTheCorpusTimezoneMovesTheAgeBoundary(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no system zoneinfo for America/Los_Angeles")
	}
	_ = la
	dir := corpus(t, map[string]string{
		"memory.jsonl": `{"type":"memory","schema_version":2,"id":"m0000020","kind":"preference","subject":"zoned","title":"Zoned","body":"x","weight":1.0,"half_life_days":10,"created":"2026-03-09","last_seen":"2026-03-09","hits":1,"source":"manual"}` + "\n",
	})
	// probeTime is 2026-03-10T12:00Z, which is still 2026-03-10 in Los Angeles;
	// at 04:00Z it would be the 9th there, and the record would be a day
	// younger. The zone is therefore part of the answer, and the adapter says
	// which one it used.
	a, _ := start(t, dir, "clara-memory",
		merge(memorySettings(), map[string]any{"timezone": "America/Los_Angeles", "debug_today": nil}))
	h, err := a.Health(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if h.Diagnostics["decay_timezone"] != "America/Los_Angeles" {
		t.Errorf("decay_timezone = %v", h.Diagnostics["decay_timezone"])
	}
	if h.Diagnostics["aged_to"] != "2026-03-10" {
		t.Errorf("aged_to = %v, want the civil date in the corpus zone", h.Diagnostics["aged_to"])
	}
}

// --- sensitivity -------------------------------------------------------------

func TestTheMemoryFloorIsConfidentialAndSignalsAreInternal(t *testing.T) {
	_, mem := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	if mem.Sensitivity != recall.SensitivityConfidential {
		t.Errorf("memory floor = %s, want confidential", mem.Sensitivity)
	}
	_, sig := start(t, signalCorpus(t), "clara-signals", signalSettings())
	if sig.Sensitivity != recall.SensitivityInternal {
		t.Errorf("signals floor = %s, want internal to match the sources they project", sig.Sensitivity)
	}
}

func TestCorrespondenceRaisesItselfAboveTheSourceFloor(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "insurance"})

	if got := find(t, res, "s0000002").Sensitivity; got != recall.SensitivityConfidential {
		t.Errorf("slack DM = %s, want confidential: the excerpt is somebody else's words", got)
	}
	if got := find(t, res, "s0000001").Sensitivity; got != recall.SensitivityInternal {
		t.Errorf("task signal = %s, want the source floor", got)
	}
}

// --- ranking and identity ----------------------------------------------------

func TestAnExactRefIsPromotedOverText(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "tasks:77aa88bb"})

	if len(res.Candidates) == 0 {
		t.Fatal("an exact ref matched nothing")
	}
	first := res.Candidates[0]
	if first.SourceRecordID != "s0000004" || !first.Exact() {
		t.Fatalf("first = %s exact=%v, want the archived signal promoted on its ref",
			first.SourceRecordID, first.Exact())
	}
}

func TestSubstringsNeverCarryTheExactSignal(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res := search(t, a, recall.SearchRequest{Query: "aa11"})
	for _, c := range res.Candidates {
		if c.Exact() {
			t.Errorf("%s claimed an exact identifier match on a substring", c.SourceRecordID)
		}
	}
}

func TestTwoInstancesOverOneStoreFingerprintTheSameRecordIdentically(t *testing.T) {
	dir := signalCorpus(t)
	one, _ := start(t, dir, "clara-signals", signalSettings())
	two, _ := start(t, dir, "clara-signals-again", signalSettings())

	a := find(t, search(t, one, recall.SearchRequest{Query: "insurance"}), "s0000001")
	b := find(t, search(t, two, recall.SearchRequest{Query: "insurance"}), "s0000001")
	if a.ContentFingerprint == "" || a.ContentFingerprint != b.ContentFingerprint {
		t.Fatalf("fingerprints %q and %q: a duplicate configuration would corroborate itself until it is corrected",
			a.ContentFingerprint, b.ContentFingerprint)
	}
	other := find(t, search(t, one, recall.SearchRequest{Query: "insurance"}), "s0000002")
	if other.ContentFingerprint == a.ContentFingerprint {
		t.Error("two different records share a fingerprint, so they would collapse into one unit")
	}
}

func TestMemoryFingerprintCoversTextLineageSensitivityAndDecayInputs(t *testing.T) {
	record := func() map[string]any {
		return map[string]any{
			"type": "memory", "schema_version": 2, "id": "m-semantic",
			"kind": "preference", "subject": "workflow", "title": "Prefer review",
			"body": "Ask for adversarial review.", "weight": 0.7,
			"half_life_days": 45.0, "created": "2026-02-01",
			"last_seen": "2026-03-01", "hits": 4, "source": "observed",
			"effect": map[string]any{"source": "tasks", "kind": "todo", "direction": 1},
			"provenance": map[string]any{
				"type": "observations-v1", "threshold": 4,
				"refs": []string{"tasks:a", "tasks:b", "tasks:c", "tasks:d"},
			},
		}
	}
	fingerprintOf := func(t *testing.T, rec map[string]any, upstream string) string {
		t.Helper()
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		dir := corpus(t, map[string]string{"memory.jsonl": string(raw) + "\n"})
		a, _ := start(t, dir, "clara-memory", merge(memorySettings(),
			map[string]any{"upstream": map[string]any{"tasks": upstream}}))
		res := search(t, a, recall.SearchRequest{})
		return find(t, res, "m-semantic").ContentFingerprint
	}

	base := fingerprintOf(t, record(), "tasks")
	cases := map[string]func(map[string]any){
		"title":     func(r map[string]any) { r["title"] = "Prefer independent review" },
		"body":      func(r map[string]any) { r["body"] = "Ask for two reviewers." },
		"weight":    func(r map[string]any) { r["weight"] = 0.8 },
		"half-life": func(r map[string]any) { r["half_life_days"] = 90.0 },
		"last-seen": func(r map[string]any) { r["last_seen"] = "2026-03-02" },
		"lineage": func(r map[string]any) {
			r["provenance"].(map[string]any)["refs"] =
				[]string{"tasks:a", "tasks:b", "tasks:c", "tasks:e"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := record()
			mutate(changed)
			if got := fingerprintOf(t, changed, "tasks"); got == base {
				t.Errorf("fingerprint stayed %q after %s changed", got, name)
			}
		})
	}
	if got := fingerprintOf(t, record(), "tasks-alternate"); got == base {
		t.Errorf("fingerprint stayed %q after derived_from lineage changed", got)
	}
}

func TestSignalFingerprintCoversTextLineageSensitivityAndObservationProjection(t *testing.T) {
	var baseRecord map[string]any
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(signalsLive), "\n")[0]), &baseRecord); err != nil {
		t.Fatal(err)
	}
	clone := func() map[string]any {
		raw, _ := json.Marshal(baseRecord)
		var copied map[string]any
		_ = json.Unmarshal(raw, &copied)
		return copied
	}
	fingerprintOf := func(t *testing.T, rec map[string]any, upstream, observation string) (string, recall.Sensitivity) {
		t.Helper()
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"signals.jsonl": string(raw) + "\n"}
		if observation != "" {
			files["observations.jsonl"] = observation + "\n"
		}
		dir := corpus(t, files)
		source, _ := rec["source"].(string)
		a, _ := start(t, dir, "clara-signals", merge(signalSettings(),
			map[string]any{"upstream": map[string]any{source: upstream}}))
		res := search(t, a, recall.SearchRequest{Query: "insurance"})
		c := find(t, res, "s0000001")
		return c.ContentFingerprint, c.Sensitivity
	}

	base, baseSensitivity := fingerprintOf(t, clone(), "tasks", "")
	for name, mutate := range map[string]func(map[string]any){
		"title":       func(r map[string]any) { r["title"] = "Renew business insurance" },
		"summary":     func(r map[string]any) { r["summary"] = "TODO — Renew business insurance" },
		"raw_excerpt": func(r map[string]any) { r["raw_excerpt"] = "@finance" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := clone()
			mutate(changed)
			if got, _ := fingerprintOf(t, changed, "tasks", ""); got == base {
				t.Errorf("fingerprint stayed %q after %s changed", got, name)
			}
		})
	}
	if got, _ := fingerprintOf(t, clone(), "tasks-alternate", ""); got == base {
		t.Errorf("fingerprint stayed %q after derived_from changed", got)
	}
	acted := `{"type":"observation","schema_version":2,"id":"o1","ref":"tasks:aa11bb22","signal_id":"s0000001","action":"acted","source":"tasks","kind":"todo","occurred_at":"2026-03-03T09:15:00Z","metadata":{}}`
	if got, _ := fingerprintOf(t, clone(), "tasks", acted); got == base {
		t.Errorf("fingerprint stayed %q after observation projection changed", got)
	}

	correspondence := clone()
	correspondence["source"] = "calendar"
	correspondence["ref"] = "calendar:aa11bb22"
	if got, sensitivity := fingerprintOf(t, correspondence, "calendar", ""); got == base {
		t.Errorf("fingerprint stayed %q after sensitivity-bearing source changed", got)
	} else if sensitivity == baseSensitivity {
		t.Errorf("sensitivity stayed %s after correspondence change", sensitivity)
	}
}

func TestDuplicateIDsResolveLastWriteForBothSearchAndExpand(t *testing.T) {
	first := `{"type":"memory","schema_version":2,"id":"same","kind":"fact","subject":"duplicate","title":"First statement","body":"alpha body","weight":1,"created":"2026-03-01","last_seen":"2026-03-01"}`
	last := `{"type":"memory","schema_version":2,"id":"same","kind":"fact","subject":"duplicate","title":"Last statement","body":"beta body","weight":1,"created":"2026-03-01","last_seen":"2026-03-01"}`
	dir := corpus(t, map[string]string{"memory.jsonl": first + "\n" + last + "\n"})
	a, _ := start(t, dir, "clara-memory", memorySettings())

	res := search(t, a, recall.SearchRequest{})
	if len(res.Candidates) != 1 || res.Candidates[0].Title != "Last statement" {
		t.Fatalf("candidates = %+v, want only the last statement", res.Candidates)
	}
	if res.Diagnostics["duplicate_records_resolved"] != 1 {
		t.Errorf("duplicate_records_resolved = %v", res.Diagnostics["duplicate_records_resolved"])
	}
	expanded, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator:  res.Candidates[0].Locator,
		Detail:   recall.DetailFull,
		Budget:   4096,
		Deadline: probeTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expanded.Content, "beta body") || strings.Contains(expanded.Content, "alpha body") {
		t.Errorf("expand disagrees with search:\n%s", expanded.Content)
	}
}

func TestCandidatesCarryTheFourTimestampsSeparately(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	c := find(t, search(t, a, recall.SearchRequest{Query: "insurance"}), "s0000004")

	if c.EventTime == nil || !c.EventTime.Equal(time.Date(2026, 1, 9, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("event_time = %v, want the upstream instant", c.EventTime)
	}
	if c.ValidFrom == nil || c.ValidFrom.Format("2006-01-02") != "2026-01-09" {
		t.Errorf("valid_from = %v, want the day Clara first held the claim", c.ValidFrom)
	}
	if c.ValidTo == nil || c.ValidTo.Format("2006-01-02") != "2026-02-06" {
		t.Errorf("valid_to = %v, want the day Clara retired it", c.ValidTo)
	}
	if c.ObservedAt == nil || c.ConfirmedAt == nil {
		t.Error("observation and confirmation were collapsed away")
	}
}

// --- freshness ---------------------------------------------------------------

// TestARewriteIsSeenAndDeletionIsHonored is why this adapter rebuilds whole: a
// byte cursor over a file Clara rewrites would keep serving a deleted record for
// ever, which docs/spec.md#index-obligations forbids.
func TestARewriteIsSeenAndDeletionIsHonored(t *testing.T) {
	dir := corpus(t, map[string]string{"memory.jsonl": memoryLive})
	a, _ := start(t, dir, "clara-memory", memorySettings())

	before := search(t, a, recall.SearchRequest{Query: "insurance"})
	if n := len(before.Candidates); n < 2 {
		t.Fatalf("candidates = %d before the rewrite", n)
	}

	// What `memory forget` does: rewrite the file without the record.
	kept := strings.Split(strings.TrimSpace(memoryLive), "\n")[0] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(kept), 0o600); err != nil {
		t.Fatal(err)
	}
	// Size changed, so the stamp differs whatever the filesystem did to mtime.

	after := search(t, a, recall.SearchRequest{Query: "insurance"})
	for _, c := range after.Candidates {
		if c.SourceRecordID == "m0000002" {
			t.Fatal("a forgotten memory is still being served")
		}
	}
}

func TestWatermarkDigestChangesWhenCountsBytesAndDatesDoNot(t *testing.T) {
	body := `{"type":"memory","schema_version":2,"id":"m1","kind":"fact","subject":"digest","title":"Alpha","body":"same","weight":1,"created":"2026-03-01","last_seen":"2026-03-01"}` + "\n"
	dir := corpus(t, map[string]string{"memory.jsonl": body})
	a, _ := start(t, dir, "clara-memory", memorySettings())
	before := search(t, a, recall.SearchRequest{}).SourceWatermark

	changed := strings.Replace(body, `"Alpha"`, `"Bravo"`, 1)
	if len(changed) != len(body) {
		t.Fatal("test rewrite changed byte count")
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(t.Context(), protocol.RefreshParams{Full: true}); err != nil {
		t.Fatal(err)
	}
	after := search(t, a, recall.SearchRequest{}).SourceWatermark
	if before == after {
		t.Fatalf("watermark stayed %q after same-size semantic content changed", before)
	}
	if !strings.Contains(before, "digest=") || !strings.Contains(after, "digest=") {
		t.Fatalf("watermarks do not carry content digests: %q / %q", before, after)
	}
}

func TestUnpinnedMemoryRebuildsAcrossTheCorpusCivilDay(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"memory","schema_version":2,"id":"fast","kind":"preference","subject":"choice-fast","title":"Choice fast","body":"choice","weight":1,"half_life_days":0.5,"created":"2026-03-10","last_seen":"2026-03-10"}`,
		`{"type":"memory","schema_version":2,"id":"stable","kind":"fact","subject":"choice-stable","title":"Choice stable","body":"choice","weight":0.7,"created":"2026-03-10","last_seen":"2026-03-10"}`,
	}, "\n") + "\n"
	dir := corpus(t, map[string]string{"memory.jsonl": body})
	now := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	a := claracorpus.New(claracorpus.Options{Clock: func() time.Time { return now }})
	if _, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "clara-memory", Location: dir,
		Settings: map[string]any{"store": "memory", "timezone": "UTC"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })

	first := search(t, a, recall.SearchRequest{Query: "choice"})
	if got := ids(first)[0]; got != "fast" {
		t.Fatalf("day-one order = %v, want fast first", ids(first))
	}
	fastBefore := find(t, first, "fast")
	if fastBefore.Metadata["age_days"] != 0 || fastBefore.SourceRevision != "gen-1" {
		t.Fatalf("day-one fast = age %v revision %s",
			fastBefore.Metadata["age_days"], fastBefore.SourceRevision)
	}

	now = now.Add(time.Hour) // 2026-03-11 in the corpus timezone; files unchanged.
	second := search(t, a, recall.SearchRequest{Query: "choice"})
	if got := ids(second)[0]; got != "stable" {
		t.Fatalf("day-two order = %v, want stable first after fast memory decays", ids(second))
	}
	fastAfter := find(t, second, "fast")
	if fastAfter.Metadata["age_days"] != 1 || fastAfter.SourceRevision != "gen-2" {
		t.Fatalf("day-two fast = age %v revision %s",
			fastAfter.Metadata["age_days"], fastAfter.SourceRevision)
	}
	if effective := fastAfter.Metadata["effective_weight"]; effective != 0.25 {
		t.Errorf("day-two effective_weight = %v, want 0.25", effective)
	}
	if fastAfter.ContentFingerprint == fastBefore.ContentFingerprint {
		t.Errorf("fingerprint stayed %q after effective weight changed", fastAfter.ContentFingerprint)
	}
}

func TestPinnedMemoryDoesNotRebuildWhenTheWallClockCrossesMidnight(t *testing.T) {
	dir := corpus(t, map[string]string{"memory.jsonl": memoryLive})
	now := time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC)
	a := claracorpus.New(claracorpus.Options{Clock: func() time.Time { return now }})
	if _, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: t.TempDir(), SourceID: "clara-memory", Location: dir,
		Settings: memorySettings(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	before := find(t, search(t, a, recall.SearchRequest{Query: "insurance"}), "m0000002")
	now = now.Add(48 * time.Hour)
	after := find(t, search(t, a, recall.SearchRequest{Query: "insurance"}), "m0000002")
	if after.SourceRevision != before.SourceRevision ||
		after.ContentFingerprint != before.ContentFingerprint ||
		after.Metadata["effective_weight"] != before.Metadata["effective_weight"] {
		t.Errorf("pinned memory drifted across wall-clock days: before=%+v after=%+v", before, after)
	}
}

func TestGenerationsAreMonotonicAcrossRestarts(t *testing.T) {
	dir := memoryCorpus(t)
	workdir := t.TempDir()
	newOne := func() *claracorpus.Adapter {
		a := claracorpus.New(claracorpus.Options{Clock: func() time.Time { return probeTime }})
		if _, err := a.Initialize(t.Context(), adapter.Config{
			ProtocolVersionMin: 1, ProtocolVersionMax: 1,
			Workdir: workdir, SourceID: "clara-memory", Location: dir,
			Settings: memorySettings(),
		}); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		return a
	}
	first := newOne()
	h1, _ := first.Health(t.Context())
	_ = first.Close()

	second := newOne()
	h2, _ := second.Health(t.Context())
	_ = second.Close()

	if h1.IndexGeneration != "gen-1" || h2.IndexGeneration != "gen-2" {
		t.Errorf("generations %q then %q: an id must never name two different builds of one workdir",
			h1.IndexGeneration, h2.IndexGeneration)
	}
	if _, err := os.Stat(filepath.Join(workdir, "cursor.json")); err != nil {
		t.Errorf("no checkpoint in the workdir: %v", err)
	}
}

func TestCheckpointFailureNeverPublishesReusableGenerationIdentity(t *testing.T) {
	record := func(title string) string {
		return `{"type":"memory","schema_version":2,"id":"m1","kind":"fact","subject":"checkpoint","title":"` +
			title + `","body":"checkpoint evidence","weight":1,"created":"2026-03-01","last_seen":"2026-03-01"}` + "\n"
	}
	dir := corpus(t, map[string]string{"memory.jsonl": record("Initial")})
	workdir := t.TempDir()
	a := claracorpus.New(claracorpus.Options{Clock: func() time.Time { return probeTime }})
	if _, err := a.Initialize(t.Context(), adapter.Config{
		ProtocolVersionMin: 1, ProtocolVersionMax: 1,
		Workdir: workdir, SourceID: "clara-memory", Location: dir,
		Settings: memorySettings(),
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	h1, err := a.Health(t.Context())
	if err != nil || h1.IndexGeneration != "gen-1" {
		t.Fatalf("first health = %+v, %v", h1, err)
	}

	checkpointPath := filepath.Join(workdir, "cursor.json")
	if err := os.Remove(checkpointPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkpointPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(record("Failed")), 0o600); err != nil {
		t.Fatal(err)
	}
	failed, err := a.Refresh(t.Context(), protocol.RefreshParams{Full: true})
	if err == nil {
		t.Fatal("checkpoint failure published changed content")
	}
	if failed.IndexGeneration != "gen-1" {
		t.Fatalf("failed refresh reported %q, want the prior durable generation", failed.IndexGeneration)
	}

	if err := os.RemoveAll(checkpointPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(record("Recovered")), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.Refresh(t.Context(), protocol.RefreshParams{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.IndexGeneration != "gen-2" {
		t.Fatalf("recovered generation = %q, want gen-2; failed content must not consume or reuse a published identity",
			recovered.IndexGeneration)
	}
	res := search(t, a, recall.SearchRequest{})
	if len(res.Candidates) != 1 || res.Candidates[0].Title != "Recovered" {
		t.Fatalf("published candidates = %+v", res.Candidates)
	}
}

func TestRefreshRebuildsAndReportsHealth(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	h, err := a.Refresh(t.Context(), protocol.RefreshParams{Full: true})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if h.IndexGeneration == "" || h.IndexConfig == "" {
		t.Error("a refresh that owns an index reported no generation identity")
	}
}

// --- expansion ---------------------------------------------------------------

func TestExpandWidensRatherThanReshapes(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	loc := recall.Locator{SourceID: "clara-signals", Local: "sig/v2/s0000001"}

	var previous string
	for _, detail := range []recall.DetailLevel{
		recall.DetailSummary, recall.DetailExcerpt, recall.DetailFull, recall.DetailContext,
	} {
		res, err := a.Expand(t.Context(), recall.ExpandRequest{
			Locator: loc, Detail: detail, Budget: 8192, Deadline: probeTime.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("expand %s: %v", detail, err)
		}
		if !strings.HasPrefix(res.Content, previous) {
			t.Fatalf("%s rewrote the previous level rather than adding to it:\n%s", detail, res.Content)
		}
		previous = res.Content
	}
}

func TestExpandTruncatesAtTheBudgetAndNamesTheBoundary(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	res, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "clara-signals", Local: "sig/v2/s0000001"},
		Detail:  recall.DetailFull, Budget: 40, Deadline: probeTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !res.Truncated || res.TruncationBoundary != "budget_bytes" {
		t.Errorf("truncated=%v boundary=%q", res.Truncated, res.TruncationBoundary)
	}
	if len(res.Content) > 40 {
		t.Errorf("content is %d bytes over a 40 byte budget", len(res.Content))
	}
}

func TestCandidatePreviewClippingNeverExceedsItsByteLimit(t *testing.T) {
	body := strings.Repeat("é", 200)
	record := `{"type":"memory","schema_version":2,"id":"wide","kind":"fact","subject":"wide","title":"Wide","body":"` +
		body + `","weight":1,"created":"2026-03-01","last_seen":"2026-03-01"}` + "\n"
	a, _ := start(t, corpus(t, map[string]string{"memory.jsonl": record}),
		"clara-memory", memorySettings())
	res := search(t, a, recall.SearchRequest{})
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d", len(res.Candidates))
	}
	excerpt := res.Candidates[0].Excerpt
	if len(excerpt) > 240 {
		t.Errorf("excerpt is %d bytes, limit 240", len(excerpt))
	}
	if !utf8.ValidString(excerpt) {
		t.Error("excerpt split a UTF-8 sequence")
	}
}

func TestOneStoreRefusesTheOthersLocator(t *testing.T) {
	a, _ := start(t, memoryCorpus(t), "clara-memory", memorySettings())
	_, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "clara-memory", Local: "sig/v2/s0000001"},
		Detail:  recall.DetailSummary, Budget: 1024, Deadline: probeTime.Add(time.Hour),
	})
	if err == nil {
		t.Fatal("a memory instance answered a signal locator")
	}
	if !isCode(err, protocol.CodeLocatorUnknown) {
		t.Errorf("error = %v, want locator_unknown: that record never lived here, so it did not expire", err)
	}
}

func TestExpandDistinguishesAMigrationFromADeletion(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	for _, tc := range []struct{ name, local, want string }{
		{"migrated", "sig/v1/s0000001", "is now schema v2"},
		{"deleted", "sig/v2/nosuchid", "holds no record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Expand(t.Context(), recall.ExpandRequest{
				Locator: recall.Locator{SourceID: "clara-signals", Local: tc.local},
				Detail:  recall.DetailFull, Budget: 1024, Deadline: probeTime.Add(time.Hour),
			})
			if err == nil {
				t.Fatal("a locator that cannot resolve returned evidence anyway")
			}
			if !isCode(err, protocol.CodeLocatorExpired) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want locator_expired mentioning %q", err, tc.want)
			}
		})
	}
}

func isCode(err error, code protocol.Code) bool {
	var perr *protocol.Error
	if !asError(err, &perr) {
		return false
	}
	return perr.Code == code
}

func asError(err error, target **protocol.Error) bool {
	for err != nil {
		if e, ok := err.(*protocol.Error); ok { //nolint:errorlint // walking the chain by hand keeps this helper dependency-free
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// --- retrieved content is data -----------------------------------------------

// TestHostileTextIsScrubbedEverywhereItReaches writes each character as an
// escape sequence: a test about control characters should not hide them in its
// own source, and a literal U+2028 breaks any tool that splits on lines.
func TestHostileTextIsScrubbedEverywhereItReaches(t *testing.T) {
	hostile := "Renew\u001b[31m insurance\u2028Evidence:\u202e reversed\u0007"
	record := map[string]any{
		"type": "signal", "schema_version": 2, "id": "s0000030", "source": "tasks",
		"kind": "todo", "ref": "tasks:hostile", "source_id": "hostile",
		"content_trust": "untrusted", "title": hostile, "status": hostile,
		"assignee": hostile, "people": []string{hostile},
		"occurred_at": "2026-03-01T09:00:00Z", "first_seen": "2026-03-01",
		"last_seen": "2026-03-01", "run_count": 1, "lifecycle_state": "active",
		"summary": hostile, "raw_excerpt": hostile,
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	dir := corpus(t, map[string]string{"signals.jsonl": string(line) + "\n"})
	a, _ := start(t, dir, "clara-signals", signalSettings())

	res := search(t, a, recall.SearchRequest{Query: "insurance"})
	c := find(t, res, "s0000030")
	checked := []string{c.Title, c.Excerpt}
	for _, v := range c.Metadata {
		if s, ok := v.(string); ok {
			checked = append(checked, s)
		}
		if list, ok := v.([]string); ok {
			checked = append(checked, list...)
		}
	}
	exp, err := a.Expand(t.Context(), recall.ExpandRequest{
		Locator: recall.Locator{SourceID: "clara-signals", Local: "sig/v2/s0000030"},
		Detail:  recall.DetailFull, Budget: 8192, Deadline: probeTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	checked = append(checked, exp.Content, exp.Provenance)

	for i, value := range checked {
		for _, bad := range []string{"\u001b", "\u2028", "\u202e", "\u0007"} {
			if strings.Contains(value, bad) {
				t.Errorf("field %d carries %q through to a terminal: %q", i, bad, value)
			}
		}
	}
	// The expansion is line-oriented, so a forged header would look like a
	// section this adapter wrote. Field values are collapsed onto one line, so
	// "Evidence:" can only ever appear inside one.
	for _, line := range strings.Split(exp.Content, "\n") {
		if strings.HasPrefix(line, "Evidence:") {
			t.Errorf("source text forged a section header:\n%s", exp.Content)
		}
	}
}

// --- cancellation ------------------------------------------------------------

func TestASearchNoticesCancellationAndDoesNotAnswer(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals",
		signalSettings(map[string]any{"debug_stall_ms": 30000}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := a.Search(ctx, recall.SearchRequest{
		Query: "insurance", Limit: 10, Deadline: probeTime.Add(time.Hour),
	})
	if err == nil {
		t.Fatal("a cancelled search answered")
	}
	if res.Outcome == recall.SearchSuccess {
		t.Errorf("outcome = %s", res.Outcome)
	}
}

func TestUseAfterCloseFails(t *testing.T) {
	a, _ := start(t, signalCorpus(t), "clara-signals", signalSettings())
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Search(t.Context(), recall.SearchRequest{
		Query: "insurance", Limit: 5, Deadline: probeTime.Add(time.Hour),
	}); err == nil {
		t.Error("a closed adapter answered from a projection nobody is maintaining")
	}
}
