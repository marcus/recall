package qmd

import (
	"strings"
	"testing"

	"github.com/marcus/recall/pkg/recall"
)

// qmd 2.5.3 accepts `--format json` on `status`, `collection show`, and
// `--version` and ignores it, so these reports are parsed as text. The rule that
// makes that safe is that a field which cannot be read is absent rather than
// defaulted: a missing count leaves coverage unknown and degrades the source,
// and a missing collection path makes it unavailable. A wording change surfaces
// as a source that cannot confirm itself.

const statusReport = `QMD Status

Index: /store/.qmd/index.sqlite
Size:  3.1 MB

Documents
  Total:    58 files indexed
  Vectors:  188 embedded
  Updated:  2h ago

Collections
  fixture (qmd://fixture/)
    Pattern:  **/*.md
    Files:    58 (updated 2h ago)
  other (qmd://other/)
    Pattern:  **/*.txt
    Files:    3 (updated 2h ago)

Examples
  # Get a document
  qmd get qmd://fixture/path/to/file.md

Models
  Embedding:   https://huggingface.co/ggml-org/embeddinggemma-300M-GGUF
  Reranking:   https://huggingface.co/ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF
  Generation:  https://huggingface.co/tobil/qmd-query-expansion-1.7B-gguf

Tips
  Add context to collections for better search results: fixture
    qmd context add qmd://<name>/ "What this collection contains"
`

func TestParseStatusReadsOnlyTheNamedCollection(t *testing.T) {
	got := parseStatus(statusReport, "fixture")
	switch {
	case !got.HasCollection:
		t.Fatal("the named collection was not found")
	case got.IndexPath != "/store/.qmd/index.sqlite":
		t.Errorf("index path = %q", got.IndexPath)
	case got.Documents != 58 || got.Vectors != 188:
		t.Errorf("counts = %d/%d, want 58/188", got.Documents, got.Vectors)
	case got.Collection.Files != 58:
		t.Errorf("collection files = %d, want 58", got.Collection.Files)
	case got.Collection.Pattern != "**/*.md":
		t.Errorf("pattern = %q, want the named collection's", got.Collection.Pattern)
	}
	// Model identities are reduced to the repository name: it identifies the
	// model, the host does not, and nothing that looks like a fetchable location
	// belongs in a health report.
	if got.Embedding != "embeddinggemma-300M-GGUF" || got.Reranker != "Qwen3-Reranker-0.6B-Q8_0-GGUF" {
		t.Errorf("models = %q / %q", got.Embedding, got.Reranker)
	}
	// The other collection's pattern and count must not bleed across.
	other := parseStatus(statusReport, "other")
	if other.Collection.Pattern != "**/*.txt" || other.Collection.Files != 3 {
		t.Errorf("other = %q/%d", other.Collection.Pattern, other.Collection.Files)
	}
	// A collection the index does not hold is absent, not empty.
	if absent := parseStatus(statusReport, "missing"); absent.HasCollection {
		t.Error("a collection qmd never listed was reported as present")
	}
}

func TestParseStatusWithoutCountsIsUnknown(t *testing.T) {
	got := parseStatus("QMD Status\n\nCollections\n  fixture (qmd://fixture/)\n    Pattern:  **/*.md\n", "fixture")
	if got.HasCounts || got.Collection.HasFiles {
		t.Fatal("counts were invented")
	}
	if watermark := indexWatermark(got); watermark != "" {
		t.Errorf("index_watermark = %q, want empty when nothing was reported", watermark)
	}
	if gen := indexGeneration("2.5.3", got, Settings{Collection: "fixture", Mode: ModeHybrid}); gen != "" {
		t.Errorf("index_generation = %q, want empty when nothing was reported", gen)
	}
}

func TestParseCollection(t *testing.T) {
	got, err := parseCollection("Collection: fixture\n  Path:     /store/corpus\n"+
		"  Pattern:  **/*.md\n  Include:  yes (default)\n", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/store/corpus" || !got.Included {
		t.Errorf("collection = %+v", got)
	}

	// qmd exits 0 and says so in prose, so this is where it becomes a failure.
	if _, err := parseCollection("Collection not found: fixture\n", "fixture"); err == nil {
		t.Error("a missing collection was accepted")
	}
	// A report about another collection is a broken contract, not a mismatch: the
	// question asked was about this one.
	if _, err := parseCollection("Collection: other\n  Path:     /store/corpus\n", "fixture"); err == nil {
		t.Error("a report about another collection was accepted")
	}
	// Without a path there is nothing to compare the configured location against,
	// which is the whole point of the check.
	if _, err := parseCollection("Collection: fixture\n  Pattern:  **/*.md\n", "fixture"); err == nil {
		t.Error("a collection with no path was accepted")
	}
}

func TestParseVersionDropsTheProgramName(t *testing.T) {
	if got := parseVersion("qmd 2.5.3\n"); got != "2.5.3" {
		t.Errorf("version = %q", got)
	}
	if got := parseVersion("\n\n2.6.0-beta\n"); got != "2.6.0-beta" {
		t.Errorf("version = %q", got)
	}
	if got := parseVersion(""); got != "" {
		t.Errorf("version = %q, want empty", got)
	}
}

// A watermark has to be comparable between two searches, between search and
// health, and between two machines indexing one corpus. This one is counts, and
// what matters here is that it carries nothing about this machine and does not
// move when an unchanged corpus is reindexed.
func TestWatermarksCarryNothingMachineLocal(t *testing.T) {
	set := Settings{Collection: "fixture", Mode: ModeHybrid, MaxCandidates: 25}
	first := parseStatus(statusReport, "fixture")
	second := parseStatus(strings.Replace(statusReport, "Updated:  2h ago", "Updated:  1s ago", 1), "fixture")

	if sourceWatermark(first, set) != sourceWatermark(second, set) {
		t.Fatal("a reindex of an unchanged corpus moved the source watermark")
	}
	if indexGeneration("2.5.3", first, set) != indexGeneration("2.5.3", second, set) {
		t.Fatal("a reindex of an unchanged corpus moved the generation")
	}
	for _, value := range []string{
		sourceWatermark(first, set),
		indexWatermark(first),
		indexGeneration("2.5.3", first, set),
	} {
		if strings.Contains(value, "/store") || strings.Contains(value, "index.sqlite") {
			t.Errorf("%q carries a machine path", value)
		}
	}
	// A changed corpus moves both.
	grown := parseStatus(strings.Replace(statusReport, "Total:    58", "Total:    59", 1), "fixture")
	if indexWatermark(first) == indexWatermark(grown) {
		t.Error("an added document did not move the index watermark")
	}
	if indexGeneration("2.5.3", first, set) == indexGeneration("2.5.3", grown, set) {
		t.Error("an added document did not move the generation")
	}
	// So does a configuration change, which is what index_config exists for.
	other := set
	other.Mode = ModeFull
	if indexGeneration("2.5.3", first, set) == indexGeneration("2.5.3", first, other) {
		t.Error("a mode change did not start a new generation")
	}
	if indexGeneration("2.5.3", first, set) == indexGeneration("2.6.0", first, set) {
		t.Error("a qmd upgrade did not start a new generation")
	}
}

func TestCheckpointProgressRequiresMonotonicComparableCounts(t *testing.T) {
	before := indexReport{
		Documents: 135, Vectors: 210, HasCounts: true,
		Collection: collectionStatus{Files: 135, HasFiles: true},
	}
	cases := []struct {
		name  string
		after recall.Health
		want  recall.CheckpointProgress
	}{
		{
			name: "advanced while still partial",
			after: recall.Health{
				RecordCount: 136, SourceWatermark: "collection=fixture files=136",
				IndexWatermark: "docs=136 vectors=210",
				Diagnostics:    map[string]any{"index_documents": 136, "index_vectors": 210},
			},
			want: recall.CheckpointAdvanced,
		},
		{
			name: "unchanged",
			after: recall.Health{
				RecordCount: 135, SourceWatermark: "collection=fixture files=135",
				IndexWatermark: "docs=135 vectors=210",
				Diagnostics:    map[string]any{"index_documents": 135, "index_vectors": 210},
			},
			want: recall.CheckpointUnchanged,
		},
		{
			name: "regressed",
			after: recall.Health{
				RecordCount: 135, SourceWatermark: "collection=fixture files=135",
				IndexWatermark: "docs=134 vectors=210",
				Diagnostics:    map[string]any{"index_documents": 134, "index_vectors": 210},
			},
			want: recall.CheckpointRegressed,
		},
		{
			name: "unknown parse",
			after: recall.Health{
				RecordCount: 136, SourceWatermark: "collection=fixture files=136",
				IndexWatermark: "docs=136 vectors=210",
				Diagnostics:    map[string]any{"index_documents": 136},
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkpointProgress(before, tc.after); got != tc.want {
				t.Fatalf("progress = %q, want %q", got, tc.want)
			}
		})
	}

	incomparable := before
	incomparable.HasCounts = false
	if got := checkpointProgress(incomparable, cases[0].after); got != "" {
		t.Fatalf("incomparable progress = %q, want omitted", got)
	}
}

func TestCoverageRules(t *testing.T) {
	report := func(documents, vectors, files int) indexReport {
		got := parseStatus(statusReport, "fixture")
		got.Documents, got.Vectors = documents, vectors
		got.Collection.Files = files
		return got
	}
	hybrid := Settings{Collection: "fixture", Mode: ModeHybrid}
	lexical := Settings{Collection: "fixture", Mode: ModeBM25}

	if got, _ := coverageOf(report(58, 188, 58), hybrid); got != coverageComplete {
		t.Errorf("a fully embedded corpus = %v", got)
	}
	if got, why := coverageOf(report(58, 40, 58), hybrid); got != coveragePartial || why == "" {
		t.Errorf("an unembedded document = %v (%q)", got, why)
	}
	if got, _ := coverageOf(report(58, 0, 58), hybrid); got != coverageNone {
		t.Errorf("no vectors at all = %v, want none", got)
	}
	// The same index answers a full-text search completely.
	if got, _ := coverageOf(report(58, 0, 58), lexical); got != coverageComplete {
		t.Errorf("bm25 over an unembedded index = %v", got)
	}
	// An empty collection is a complete boundary over nothing; unknown here would
	// make every honest "no such document" degrade.
	if got, _ := coverageOf(report(0, 0, 0), hybrid); got != coverageComplete {
		t.Errorf("an empty collection = %v", got)
	}
	missing := parseStatus(statusReport, "absent")
	if got, why := coverageOf(missing, hybrid); got != coverageUnknown || why == "" {
		t.Errorf("an unlisted collection = %v (%q)", got, why)
	}
}

func TestCheckCollectionPathComparesResolvedDirectories(t *testing.T) {
	if err := checkCollectionPath(collectionReport{Name: "fixture", Path: "/store/corpus"},
		"/store/corpus"); err != nil {
		t.Errorf("an agreeing configuration was refused: %v", err)
	}
	// Trailing separators and dot segments are the same directory.
	if err := checkCollectionPath(collectionReport{Name: "fixture", Path: "/store/corpus/"},
		"/store/./corpus"); err != nil {
		t.Errorf("one directory spelled two ways was refused: %v", err)
	}
	err := checkCollectionPath(collectionReport{Name: "fixture", Path: "/elsewhere/other"},
		"/store/corpus")
	if err == nil {
		t.Fatal("a collection indexing another tree was accepted")
	}
	if strings.Contains(err.Error(), "/elsewhere") || strings.Contains(err.Error(), "/store") {
		t.Errorf("the diagnostic carries an absolute path: %v", err)
	}
	if err := checkCollectionPath(collectionReport{Name: "fixture", Path: "/store/corpus"}, ""); err == nil {
		t.Fatal("a source with no configured location was accepted")
	}
}

func TestStoreIdentityIsOpaqueAndOptional(t *testing.T) {
	first := storeIdentity("/store/.qmd/index.sqlite", "fixture")
	if first == "" || strings.Contains(first, "store") {
		t.Fatalf("store identity = %q", first)
	}
	if first != storeIdentity("/store/.qmd/index.sqlite", "fixture") {
		t.Error("one store reported two identities")
	}
	if first == storeIdentity("/store/.qmd/index.sqlite", "other") {
		t.Error("two collections of one index reported one identity")
	}
	if first == storeIdentity("/elsewhere/.qmd/index.sqlite", "fixture") {
		t.Error("two indexes reported one identity")
	}
	// Nothing to name means no claim, never a partial one.
	if storeIdentity("", "fixture") != "" || storeIdentity("/store/index.sqlite", "") != "" {
		t.Error("an unresolvable store was still claimed")
	}
}
